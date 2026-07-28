package solar

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"src.solsynth.dev/sosys/elecpostal/internal/logging"
)

const (
	heartbeatInterval           = 60 * time.Second
	subscriptionRefreshInterval = 4 * time.Minute
	maxReconnectDelay           = 30 * time.Second
)

// Manager maintains a Solar Network websocket connection for real-time events.
type Manager struct {
	baseURL     string
	accountName string
	client      *Client

	mu      sync.RWMutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewManager creates a new Solar Network websocket manager.
func NewManager(baseURL, accountName, accessToken string) *Manager {
	return &Manager{
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		accountName: strings.TrimSpace(accountName),
		client:      NewClient(baseURL, accessToken),
	}
}

// Start connects to the Solar Network websocket and starts the event loop.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	if m.baseURL == "" || m.client.accessToken == "" {
		return nil
	}

	m.ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(m.ctx)
	}()
	m.started = true
	return nil
}

// Stop shuts down the websocket connection.
func (m *Manager) Stop(context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	m.started = false
	m.mu.Unlock()
	return nil
}

func (m *Manager) run(ctx context.Context) {
	delay := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}

		if err := m.connectAndServe(ctx); err != nil && ctx.Err() == nil {
			logging.Log.Error().Err(err).Msg("solar websocket connection ended")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(withJitter(delay)):
		}

		delay *= 2
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}
	}
}

func (m *Manager) connectAndServe(ctx context.Context) error {
	wsURL := strings.TrimPrefix(m.baseURL, "http://")
	wsURL = strings.TrimPrefix(wsURL, "https://")
	if strings.HasPrefix(m.baseURL, "https://") {
		wsURL = "wss://" + wsURL + "/ws"
	} else {
		wsURL = "ws://" + wsURL + "/ws"
	}

	cfg, err := websocket.NewConfig(wsURL, m.baseURL)
	if err != nil {
		return err
	}
	cfg.Header = http.Header{}
	cfg.Header.Set("Authorization", "Bearer "+m.client.accessToken)

	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return err
	}
	defer conn.Close()

	logging.Log.Info().Str("account", m.accountName).Msg("solar websocket connected")

	return m.serveLoop(ctx, conn)
}

func (m *Manager) serveLoop(ctx context.Context, conn *websocket.Conn) error {
	receiveCh := make(chan Packet)
	errCh := make(chan error, 1)

	go m.readLoop(conn, receiveCh, errCh)

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt := <-receiveCh:
			logging.Log.Debug().Str("type", pkt.Type).Msg("received solar websocket packet")
			if err := m.handlePacket(pkt); err != nil {
				return err
			}
		case err := <-errCh:
			return err
		case <-heartbeatTicker.C:
			if err := m.sendPacket(conn, Packet{Type: "ping"}); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) readLoop(conn *websocket.Conn, out chan<- Packet, errCh chan<- error) {
	for {
		var raw string
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			errCh <- err
			return
		}

		var pkt Packet
		if err := json.Unmarshal([]byte(raw), &pkt); err != nil {
			errCh <- fmt.Errorf("decode solar websocket packet: %w", err)
			return
		}
		out <- pkt
	}
}

func (m *Manager) handlePacket(pkt Packet) error {
	switch pkt.Type {
	case "pong":
		return nil
	case "error", "error.dupe":
		return fmt.Errorf("solar websocket error: %s", pkt.ErrorMessage)
	default:
		logging.Log.Debug().Str("type", pkt.Type).Msg("ignoring unsupported solar websocket packet")
		return nil
	}
}

func (m *Manager) sendPacket(conn *websocket.Conn, pkt Packet) error {
	raw, err := json.Marshal(pkt)
	if err != nil {
		return err
	}
	return websocket.Message.Send(conn, string(raw))
}

func withJitter(delay time.Duration) time.Duration {
	jitter := time.Duration(rand.Intn(200)-100) * time.Millisecond
	next := delay + jitter
	if next < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	return next
}
