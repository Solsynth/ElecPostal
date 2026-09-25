package smtp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"gorm.io/datatypes"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type deliveryJob struct {
	ID                   string                                   `json:"id"`
	MessageID            string                                   `json:"message_id"`
	FromAddress          string                                   `json:"from_address"`
	FromName             string                                   `json:"from_name"`
	Subject              string                                   `json:"subject"`
	Body                 string                                   `json:"body"`
	ContentType          string                                   `json:"content_type"`
	OmitContentType      bool                                     `json:"omit_content_type,omitempty"`
	To                   []service.RecipientInput                 `json:"to"`
	Cc                   []service.RecipientInput                 `json:"cc"`
	Recipients           []recipient                              `json:"recipients"`
	AttachmentReferences map[string][]service.AttachmentReference `json:"attachment_references,omitempty"`
	Authentication       datatypes.JSON                           `json:"authentication,omitempty"`
	EnvelopeFrom         string                                   `json:"envelope_from"`
	ReceivedAt           time.Time                                `json:"received_at"`
	TransientAttachments []service.IncomingAttachment             `json:"-"`
}

func newDeliveryJob(message parsedMessage, envelopeFrom string, recipients []recipient) deliveryJob {
	job := deliveryJob{
		ID: uuid.NewString(), MessageID: message.id, FromAddress: message.fromAddress,
		FromName: message.fromName, Subject: message.subject, Body: message.body,
		ContentType: message.contentType, OmitContentType: message.omitContentType,
		To: message.to, Cc: message.cc,
		Recipients: recipients, ReceivedAt: time.Now(), EnvelopeFrom: envelopeFrom,
	}
	for _, attachment := range message.attachments {
		job.TransientAttachments = append(job.TransientAttachments, service.IncomingAttachment{
			Filename: attachment.filename, MimeType: attachment.mimeType,
			Size: int64(len(attachment.content)), ContentID: attachment.contentID,
			Disposition: attachment.disposition, Content: bytes.NewReader(attachment.content),
		})
	}
	return job
}

type legacyQueuedAttachment struct {
	Filename    string `json:"filename"`
	MimeType    string `json:"mime_type"`
	ContentID   string `json:"content_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Content     []byte `json:"content"`
}

type legacyDeliveryJob struct {
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
	Attachments    []legacyQueuedAttachment `json:"attachments"`
	Authentication datatypes.JSON           `json:"authentication,omitempty"`
	RawSource      []byte                   `json:"raw_source"`
	EnvelopeFrom   string                   `json:"envelope_from"`
	ReceivedAt     time.Time                `json:"received_at"`
}

func decodeLegacyDeliveryJob(data []byte) (deliveryJob, bool, error) {
	var legacy legacyDeliveryJob
	if err := json.Unmarshal(data, &legacy); err != nil {
		return deliveryJob{}, false, err
	}
	if len(legacy.RawSource) == 0 && len(legacy.Attachments) == 0 {
		return deliveryJob{}, false, nil
	}
	job := deliveryJob{ID: legacy.ID, MessageID: legacy.MessageID, FromAddress: legacy.FromAddress, FromName: legacy.FromName, Subject: legacy.Subject, Body: legacy.Body, ContentType: legacy.ContentType, To: legacy.To, Cc: legacy.Cc, Recipients: legacy.Recipients, Authentication: legacy.Authentication, EnvelopeFrom: legacy.EnvelopeFrom, ReceivedAt: legacy.ReceivedAt}
	if len(legacy.RawSource) > 0 {
		envelopeRecipients := make([]mailmime.Recipient, 0, len(legacy.Recipients))
		for _, recipient := range legacy.Recipients {
			envelopeRecipients = append(envelopeRecipients, mailmime.Recipient{Address: recipient.Address, Kind: "to"})
		}
		parsed, err := mailmime.ParseMessage(legacy.RawSource, legacy.EnvelopeFrom, envelopeRecipients)
		if err != nil {
			return deliveryJob{}, false, err
		}
		job.FromAddress, job.FromName, job.Subject, job.Body, job.ContentType = parsed.FromAddress, parsed.FromName, parsed.Subject, parsed.Body, parsed.BodyType
		job.OmitContentType = parsed.OmitContentType
		job.To, job.Cc = nil, nil
		for _, recipient := range parsed.To {
			job.To = append(job.To, service.RecipientInput{Address: recipient.Address, Name: recipient.Name, Kind: recipient.Kind})
		}
		for _, recipient := range parsed.Cc {
			job.Cc = append(job.Cc, service.RecipientInput{Address: recipient.Address, Name: recipient.Name, Kind: recipient.Kind})
		}
		for _, attachment := range parsed.Attachments {
			content, err := io.ReadAll(attachment.Content)
			if err != nil {
				return deliveryJob{}, false, err
			}
			job.TransientAttachments = append(job.TransientAttachments, service.IncomingAttachment{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Content: bytes.NewReader(content)})
		}
	} else {
		for _, attachment := range legacy.Attachments {
			job.TransientAttachments = append(job.TransientAttachments, service.IncomingAttachment{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: int64(len(attachment.Content)), ContentID: attachment.ContentID, Disposition: attachment.Disposition, Content: bytes.NewReader(attachment.Content)})
		}
	}
	return job, true, nil
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
	if len(job.TransientAttachments) > 0 {
		stager, ok := q.backend.(attachmentStager)
		if !ok {
			return fmt.Errorf("attachment staging is not configured")
		}
		job.AttachmentReferences = map[string][]service.AttachmentReference{}
		for _, recipient := range job.Recipients {
			if recipient.MailboxID == "" {
				continue
			}
			if _, exists := job.AttachmentReferences[recipient.MailboxID]; exists {
				continue
			}
			references, err := stager.StageIncomingAttachments(ctx, recipient.MailboxID, job.TransientAttachments)
			if err != nil {
				deleteStagedAttachments(ctx, q.backend, job.AttachmentReferences)
				return err
			}
			job.AttachmentReferences[recipient.MailboxID] = references
		}
		job.TransientAttachments = nil
	}
	payload, err := json.Marshal(job)
	if err != nil {
		deleteStagedAttachments(ctx, q.backend, job.AttachmentReferences)
		return fmt.Errorf("encode SMTP delivery job: %w", err)
	}
	msg := nats.NewMsg(q.cfg.Subject)
	msg.Data = payload
	msg.Header.Set(nats.MsgIdHdr, job.ID)
	if _, err := q.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		deleteStagedAttachments(ctx, q.backend, job.AttachmentReferences)
		return fmt.Errorf("persist SMTP delivery job: %w", err)
	}
	return nil
}

func (q *NATSQueue) handle(msg *nats.Msg) {
	q.wg.Add(1)
	defer q.wg.Done()
	var job deliveryJob
	legacy, isLegacy, legacyErr := decodeLegacyDeliveryJob(msg.Data)
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		if legacyErr != nil || !isLegacy {
			if legacyErr == nil {
				legacyErr = err
			}
			logging.Log.Error().Err(legacyErr).Msg("discarding malformed SMTP NATS job")
			_ = msg.Term()
			return
		}
		job = legacy
	} else if isLegacy {
		job = legacy
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

type attachmentStager interface {
	StageIncomingAttachments(context.Context, string, []service.IncomingAttachment) ([]service.AttachmentReference, error)
}

func deleteStagedAttachments(ctx context.Context, backend Backend, refs map[string][]service.AttachmentReference) {
	store, ok := backend.(interface {
		DeleteAttachment(context.Context, string) error
	})
	if !ok {
		return
	}
	seen := map[string]struct{}{}
	for _, references := range refs {
		for _, reference := range references {
			id := strings.TrimSpace(reference.StorageKey)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			_ = store.DeleteAttachment(ctx, id)
		}
	}
}

func deliverJob(ctx context.Context, backend Backend, job deliveryJob) error {
	unique := map[string]struct{}{}
	for _, recipient := range job.Recipients {
		if recipient.MailboxID == "" {
			continue
		}
		if _, seen := unique[recipient.MailboxID]; seen {
			continue
		}
		unique[recipient.MailboxID] = struct{}{}
		references := job.AttachmentReferences[recipient.MailboxID]
		transient := []service.IncomingAttachment(nil)
		if len(references) == 0 && len(job.TransientAttachments) > 0 {
			if stager, ok := backend.(attachmentStager); ok {
				var err error
				references, err = stager.StageIncomingAttachments(ctx, recipient.MailboxID, job.TransientAttachments)
				if err != nil {
					return err
				}
			} else {
				transient = job.TransientAttachments
			}
		}
		deliveredTo := make([]string, 0)
		dmarcIntake := false
		for _, candidate := range job.Recipients {
			if candidate.MailboxID == recipient.MailboxID {
				deliveredTo = append(deliveredTo, candidate.Address)
				if isDmarcRecipient(candidate.Address) {
					dmarcIntake = true
				}
			}
		}
		if _, err := backend.ReceiveEmail(ctx, service.ReceiveEmailInput{
			MailboxID: recipient.MailboxID, MessageID: job.MessageID, FromAddress: job.FromAddress, FromName: job.FromName,
			Subject: job.Subject, Body: job.Body, ContentType: job.ContentType,
			OmitContentType: job.OmitContentType, To: job.To,
			Cc: job.Cc, Attachments: transient, AttachmentReferences: references, SentAt: &job.ReceivedAt,
			Authentication: job.Authentication, EnvelopeFrom: job.EnvelopeFrom,
			DeliveredTo: deliveredTo, DmarcIntake: dmarcIntake,
		}); err != nil {
			return err
		}
	}
	logging.Log.Info().Str("smtp_message_id", job.MessageID).Int("recipient_count", len(unique)).Msg("SMTP queued message delivered")
	return nil
}
func isDmarcRecipient(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	at := strings.LastIndex(address, "@")
	return at > 0 && address[:at] == "dmarc"
}
