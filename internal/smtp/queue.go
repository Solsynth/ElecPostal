package smtp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"gorm.io/datatypes"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type deliveryJob struct {
	ID             string                   `json:"id"`
	MessageID      string                   `json:"message_id"`
	FromAddress    string                   `json:"from_address"`
	FromName       string                   `json:"from_name"`
	Subject        string                   `json:"subject"`
	Body           string                   `json:"body"`
	ContentType    string                   `json:"content_type"`
	To             []service.RecipientInput `json:"to"`
	Cc             []service.RecipientInput `json:"cc"`
	Recipients     []recipient              `json:"recipients"`
	Attachments    []queuedAttachment       `json:"attachments"`
	Authentication datatypes.JSON           `json:"authentication,omitempty"`
	ReceivedAt     time.Time                `json:"received_at"`
}

type queuedAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Content  []byte `json:"content"`
}

func newDeliveryJob(message parsedMessage, recipients []recipient) deliveryJob {
	job := deliveryJob{ID: uuid.NewString(), MessageID: message.id, FromAddress: message.fromAddress, FromName: message.fromName, Subject: message.subject, Body: message.body, ContentType: message.contentType, To: message.to, Cc: message.cc, Recipients: recipients, ReceivedAt: time.Now()}
	for _, attachment := range message.attachments {
		job.Attachments = append(job.Attachments, queuedAttachment{Filename: attachment.filename, MimeType: attachment.mimeType, Content: attachment.content})
	}
	return job
}

type inlineDelivery struct{ backend Backend }

func (d inlineDelivery) Enqueue(ctx context.Context, job deliveryJob) error {
	return deliverJob(ctx, d.backend, job)
}

// NATSQueue uses a JetStream work-queue stream. Publish acknowledgements are
// the SMTP durability boundary; jobs are ACKed only after all local mailbox
// copies and their attachment uploads have completed.
type NATSQueue struct {
	backend Backend
	cfg     config.NATSConfig
	nc      *nats.Conn
	js      nats.JetStreamContext
	sub     *nats.Subscription
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewNATSQueue(cfg config.NATSConfig, backend Backend) (*NATSQueue, error) {
	if strings.TrimSpace(cfg.Target) == "" {
		return nil, fmt.Errorf("NATS target is required")
	}
	if backend == nil {
		return nil, fmt.Errorf("mail backend is required")
	}
	nc, err := nats.Connect(cfg.Target, nats.Name("elecpostal-smtp"))
	if err != nil {
		return nil, fmt.Errorf("connect NATS: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("initialize JetStream: %w", err)
	}
	if cfg.Stream == "" {
		cfg.Stream = "ELECPOSTAL_INBOUND"
	}
	if cfg.Subject == "" {
		cfg.Subject = "elecpostal.smtp.inbound"
	}
	if cfg.Consumer == "" {
		cfg.Consumer = "elecpostal-smtp-workers"
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: cfg.Stream, Subjects: []string{cfg.Subject}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage}); err != nil {
		if _, infoErr := js.StreamInfo(cfg.Stream); infoErr != nil {
			nc.Close()
			return nil, fmt.Errorf("create inbound SMTP stream: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &NATSQueue{backend: backend, cfg: cfg, nc: nc, js: js, ctx: ctx, cancel: cancel}, nil
}

func (q *NATSQueue) Start() error {
	var err error
	q.sub, err = q.js.QueueSubscribe(q.cfg.Subject, q.cfg.Consumer, q.handle, nats.Durable(q.cfg.Consumer), nats.ManualAck(), nats.AckExplicit(), nats.DeliverAll(), nats.MaxAckPending(q.cfg.Workers))
	if err != nil {
		return fmt.Errorf("subscribe inbound SMTP worker: %w", err)
	}
	logging.Log.Info().Str("stream", q.cfg.Stream).Str("subject", q.cfg.Subject).Int("workers", q.cfg.Workers).Msg("SMTP NATS delivery queue started")
	return nil
}

func (q *NATSQueue) Enqueue(ctx context.Context, job deliveryJob) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("encode SMTP delivery job: %w", err)
	}
	msg := nats.NewMsg(q.cfg.Subject)
	msg.Data = payload
	msg.Header.Set(nats.MsgIdHdr, job.ID)
	if _, err := q.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("persist SMTP delivery job: %w", err)
	}
	return nil
}

func (q *NATSQueue) handle(msg *nats.Msg) {
	q.wg.Add(1)
	defer q.wg.Done()
	var job deliveryJob
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		logging.Log.Error().Err(err).Msg("discarding malformed SMTP NATS job")
		_ = msg.Term()
		return
	}
	if err := deliverJob(q.ctx, q.backend, job); err != nil {
		logging.Log.Warn().Err(err).Str("smtp_message_id", job.MessageID).Msg("SMTP queued delivery failed; retrying")
		_ = msg.NakWithDelay(10 * time.Second)
		return
	}
	if err := msg.Ack(); err != nil {
		logging.Log.Warn().Err(err).Str("smtp_message_id", job.MessageID).Msg("ack SMTP queued delivery")
	}
}

func (q *NATSQueue) Close() error {
	q.cancel()
	if q.sub != nil {
		_ = q.sub.Unsubscribe()
	}
	q.wg.Wait()
	q.nc.Drain()
	q.nc.Close()
	return nil
}

func deliverJob(ctx context.Context, backend Backend, job deliveryJob) error {
	unique := map[string]struct{}{}
	for _, recipient := range job.Recipients {
		if _, seen := unique[recipient.mailboxID]; seen {
			continue
		}
		unique[recipient.mailboxID] = struct{}{}
		attachments := make([]service.IncomingAttachment, 0, len(job.Attachments))
		for _, attachment := range job.Attachments {
			attachments = append(attachments, service.IncomingAttachment{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: int64(len(attachment.Content)), Content: bytes.NewReader(attachment.Content)})
		}
		if _, err := backend.ReceiveEmail(ctx, service.ReceiveEmailInput{MailboxID: recipient.mailboxID, FromAddress: job.FromAddress, FromName: job.FromName, Subject: job.Subject, Body: job.Body, ContentType: job.ContentType, To: job.To, Cc: job.Cc, Attachments: attachments, SentAt: &job.ReceivedAt, Authentication: job.Authentication}); err != nil {
			return err
		}
	}
	logging.Log.Info().Str("smtp_message_id", job.MessageID).Int("recipient_count", len(unique)).Msg("SMTP queued message delivered")
	return nil
}
