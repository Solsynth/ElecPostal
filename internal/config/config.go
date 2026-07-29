package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	App        AppConfig        `mapstructure:"app"`
	HTTP       HTTPConfig       `mapstructure:"http"`
	GRPC       GRPCConfig       `mapstructure:"grpc"`
	Database   DatabaseConfig   `mapstructure:"database"`
	Redis      RedisConfig      `mapstructure:"redis"`
	NATS       NATSConfig       `mapstructure:"nats"`
	Auth       AuthConfig       `mapstructure:"auth"`
	FileSystem FileSystemConfig `mapstructure:"filesystem"`
	Workspace  WorkspaceConfig  `mapstructure:"workspace"`
	Mail       MailConfig       `mapstructure:"mail"`
	Ring       RingConfig       `mapstructure:"ring"`
	WebSocket  WebSocketConfig  `mapstructure:"websocket"`
	Sentry     SentryConfig     `mapstructure:"sentry"`
}

type AppConfig struct {
	Name string `mapstructure:"name"`
}

type HTTPConfig struct {
	Port string `mapstructure:"port"`
}

type GRPCConfig struct {
	Port     string `mapstructure:"port"`
	UseTLS   bool   `mapstructure:"useTLS"`
	CertFile string `mapstructure:"certFile"`
	KeyFile  string `mapstructure:"keyFile"`
}

type DatabaseConfig struct {
	DSN string `mapstructure:"dsn"`
}

type RedisConfig struct {
	Addr string `mapstructure:"addr"`
}

// NATSConfig configures durable JetStream ingestion for inbound SMTP.
// An empty target retains synchronous delivery for development only.
type NATSConfig struct {
	Target   string `mapstructure:"target"`
	Stream   string `mapstructure:"stream"`
	Subject  string `mapstructure:"subject"`
	Consumer string `mapstructure:"consumer"`
	Workers  int    `mapstructure:"workers"`
}

type AuthConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

// FileSystemConfig configures the FileSystem gRPC service used for email
// attachment contents. It is deliberately separate from the public HTTP URL:
// attachments are uploaded server-to-server on behalf of the mailbox owner.
type FileSystemConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

// WorkspaceConfig configures the Workspace gRPC endpoint used to associate
// mailboxes with workspaces and to read the plan storage quota.
type WorkspaceConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

// MailConfig contains the network-facing mail protocol listeners and the
// outbound relay. Protocol ports default to the registered standards; TLS mode
// is explicit so deployments can use STARTTLS or implicit TLS where required.
type MailConfig struct {
	Domain     string               `mapstructure:"domain"`
	Relay      RelayConfig          `mapstructure:"relay"`
	SMTP       ListenerConfig       `mapstructure:"smtp"`
	Submission ListenerConfig       `mapstructure:"submission"`
	IMAP       ListenerConfig       `mapstructure:"imap"`
	POP3       ListenerConfig       `mapstructure:"pop3"`
	SendLimits MailSendLimitsConfig `mapstructure:"sendLimits"`
}

// MailSendLimitConfig configures outgoing message limits for one plan.
// A value of zero disables the corresponding limit.
type MailSendLimitConfig struct {
	MailboxDaily     int64 `mapstructure:"mailboxDaily"`
	MailboxMonthly   int64 `mapstructure:"mailboxMonthly"`
	WorkspaceDaily   int64 `mapstructure:"workspaceDaily"`
	WorkspaceMonthly int64 `mapstructure:"workspaceMonthly"`
}

type MailSendLimitsConfig struct {
	Free       MailSendLimitConfig `mapstructure:"free"`
	Pro        MailSendLimitConfig `mapstructure:"pro"`
	Enterprise MailSendLimitConfig `mapstructure:"enterprise"`
}

type RelayConfig struct {
	Adapter       string `mapstructure:"adapter"` // direct-smtp, ses, or another registered adapter
	Host          string `mapstructure:"host"`
	Port          string `mapstructure:"port"`
	Username      string `mapstructure:"username"`
	Password      string `mapstructure:"password"`
	Region        string `mapstructure:"region"`
	InboundHost   string `mapstructure:"inboundHost"`
	TLSMode       string `mapstructure:"tlsMode"` // starttls, implicit, disabled
	TLSHost       string `mapstructure:"tlsHost"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

type ListenerConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	Host            string `mapstructure:"host"`
	Port            string `mapstructure:"port"`
	TLSMode         string `mapstructure:"tlsMode"` // starttls, implicit, disabled
	CertFile        string `mapstructure:"certFile"`
	KeyFile         string `mapstructure:"keyFile"`
	TLSSkipVerify   bool   `mapstructure:"tlsSkipVerify"`
	MaxMessageBytes int64  `mapstructure:"maxMessageBytes"`
	MaxRecipients   int    `mapstructure:"maxRecipients"`
	RequireAuth     bool   `mapstructure:"requireAuth"`
}

type RingConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

type WebSocketConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

type SentryConfig struct {
	DSN              string  `mapstructure:"dsn"`
	TracesSampleRate float64 `mapstructure:"tracesSampleRate"`
	Environment      string  `mapstructure:"environment"`
	Release          string  `mapstructure:"release"`
}

func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("toml")

	v.SetDefault("app.name", "ElecPostal")
	v.SetDefault("http.port", "8080")
	v.SetDefault("grpc.port", "9090")
	v.SetDefault("grpc.useTLS", false)
	v.SetDefault("grpc.certFile", "")
	v.SetDefault("grpc.keyFile", "")
	v.SetDefault("database.dsn", "")
	v.SetDefault("redis.addr", "")
	v.SetDefault("nats.target", "")
	v.SetDefault("nats.stream", "ELECPOSTAL_INBOUND")
	v.SetDefault("nats.subject", "elecpostal.smtp.inbound")
	v.SetDefault("nats.consumer", "elecpostal-smtp-workers")
	v.SetDefault("nats.workers", 8)
	v.SetDefault("auth.target", "")
	v.SetDefault("auth.useTLS", false)
	v.SetDefault("auth.tlsSkipVerify", false)
	v.SetDefault("filesystem.target", "")
	v.SetDefault("filesystem.useTLS", false)
	v.SetDefault("filesystem.tlsSkipVerify", false)
	v.SetDefault("workspace.target", "")
	v.SetDefault("workspace.useTLS", false)
	v.SetDefault("workspace.tlsSkipVerify", false)
	v.SetDefault("mail.domain", "")
	v.SetDefault("mail.sendLimits.free.mailboxDaily", 100)
	v.SetDefault("mail.sendLimits.free.mailboxMonthly", 2000)
	v.SetDefault("mail.sendLimits.free.workspaceDaily", 100)
	v.SetDefault("mail.sendLimits.free.workspaceMonthly", 2000)
	v.SetDefault("mail.sendLimits.pro.mailboxDaily", 1000)
	v.SetDefault("mail.sendLimits.pro.mailboxMonthly", 20000)
	v.SetDefault("mail.sendLimits.pro.workspaceDaily", 3000)
	v.SetDefault("mail.sendLimits.pro.workspaceMonthly", 60000)
	v.SetDefault("mail.sendLimits.enterprise.mailboxDaily", 5000)
	v.SetDefault("mail.sendLimits.enterprise.mailboxMonthly", 100000)
	v.SetDefault("mail.sendLimits.enterprise.workspaceDaily", 25000)
	v.SetDefault("mail.sendLimits.enterprise.workspaceMonthly", 500000)
	v.SetDefault("mail.relay.adapter", "")
	v.SetDefault("mail.relay.port", "587")
	v.SetDefault("mail.relay.tlsMode", "starttls")
	v.SetDefault("mail.smtp.port", "25")
	v.SetDefault("mail.smtp.tlsMode", "starttls")
	v.SetDefault("mail.smtp.maxMessageBytes", 25*1024*1024)
	v.SetDefault("mail.smtp.maxRecipients", 100)
	v.SetDefault("mail.submission.port", "587")
	v.SetDefault("mail.submission.tlsMode", "starttls")
	v.SetDefault("mail.submission.maxMessageBytes", 25*1024*1024)
	v.SetDefault("mail.submission.maxRecipients", 100)
	v.SetDefault("mail.submission.requireAuth", true)
	v.SetDefault("mail.imap.port", "143")
	v.SetDefault("mail.imap.tlsMode", "starttls")
	v.SetDefault("mail.pop3.port", "110")
	v.SetDefault("mail.pop3.tlsMode", "starttls")
	v.SetDefault("ring.target", "")
	v.SetDefault("ring.useTLS", false)
	v.SetDefault("ring.tlsSkipVerify", false)
	v.SetDefault("websocket.target", "")
	v.SetDefault("websocket.useTLS", false)
	v.SetDefault("websocket.tlsSkipVerify", false)
	v.SetDefault("sentry.dsn", "")
	v.SetDefault("sentry.tracesSampleRate", 0.01)
	v.SetDefault("sentry.environment", "")
	v.SetDefault("sentry.release", "")

	if configPath != "" {
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	applyEnvOverrides(v)

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	return &cfg, nil
}

func applyEnvOverrides(v *viper.Viper) {
	setEnvIfPresent(v, "database.dsn", "DATABASE_DSN")
	setEnvIfPresent(v, "auth.target", "AUTH_TARGET")
	setEnvIfPresent(v, "filesystem.target", "FILESYSTEM_TARGET")
	setEnvIfPresent(v, "workspace.target", "WORKSPACE_TARGET")
	setEnvIfPresent(v, "nats.target", "NATS_URL")
	setEnvIfPresent(v, "mail.domain", "MAIL_DOMAIN")
	setEnvIfPresent(v, "mail.relay.adapter", "MAIL_RELAY_ADAPTER")
	setEnvIfPresent(v, "mail.relay.host", "MAIL_RELAY_HOST")
	setEnvIfPresent(v, "mail.relay.port", "MAIL_RELAY_PORT")
	setEnvIfPresent(v, "mail.relay.username", "MAIL_RELAY_USERNAME")
	setEnvIfPresent(v, "mail.relay.password", "MAIL_RELAY_PASSWORD")
	setEnvIfPresent(v, "mail.relay.region", "MAIL_RELAY_REGION")
	setEnvIfPresent(v, "mail.relay.inboundHost", "MAIL_RELAY_INBOUND_HOST")
	setEnvIfPresent(v, "ring.target", "RING_TARGET")
	setEnvIfPresent(v, "websocket.target", "WEBSOCKET_TARGET")
}

func setEnvIfPresent(v *viper.Viper, key, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		v.Set(key, value)
	}
}
