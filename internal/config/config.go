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
	Auth       AuthConfig       `mapstructure:"auth"`
	FileSystem FileSystemConfig `mapstructure:"filesystem"`
	Workspace  WorkspaceConfig  `mapstructure:"workspace"`
	Mail       MailConfig       `mapstructure:"mail"`
	Ring       RingConfig       `mapstructure:"ring"`
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
	Domain string         `mapstructure:"domain"`
	Relay  RelayConfig    `mapstructure:"relay"`
	SMTP   ListenerConfig `mapstructure:"smtp"`
	IMAP   ListenerConfig `mapstructure:"imap"`
	POP3   ListenerConfig `mapstructure:"pop3"`
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
	Enabled       bool   `mapstructure:"enabled"`
	Host          string `mapstructure:"host"`
	Port          string `mapstructure:"port"`
	TLSMode       string `mapstructure:"tlsMode"` // starttls, implicit, disabled
	CertFile      string `mapstructure:"certFile"`
	KeyFile       string `mapstructure:"keyFile"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}

type RingConfig struct {
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
	v.SetDefault("mail.relay.adapter", "")
	v.SetDefault("mail.relay.port", "587")
	v.SetDefault("mail.relay.tlsMode", "starttls")
	v.SetDefault("mail.smtp.port", "587")
	v.SetDefault("mail.smtp.tlsMode", "starttls")
	v.SetDefault("mail.imap.port", "143")
	v.SetDefault("mail.imap.tlsMode", "starttls")
	v.SetDefault("mail.pop3.port", "110")
	v.SetDefault("mail.pop3.tlsMode", "starttls")
	v.SetDefault("ring.target", "")
	v.SetDefault("ring.useTLS", false)
	v.SetDefault("ring.tlsSkipVerify", false)
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
	setEnvIfPresent(v, "mail.domain", "MAIL_DOMAIN")
	setEnvIfPresent(v, "mail.relay.adapter", "MAIL_RELAY_ADAPTER")
	setEnvIfPresent(v, "mail.relay.host", "MAIL_RELAY_HOST")
	setEnvIfPresent(v, "mail.relay.port", "MAIL_RELAY_PORT")
	setEnvIfPresent(v, "mail.relay.username", "MAIL_RELAY_USERNAME")
	setEnvIfPresent(v, "mail.relay.password", "MAIL_RELAY_PASSWORD")
	setEnvIfPresent(v, "mail.relay.region", "MAIL_RELAY_REGION")
	setEnvIfPresent(v, "mail.relay.inboundHost", "MAIL_RELAY_INBOUND_HOST")
	setEnvIfPresent(v, "ring.target", "RING_TARGET")
}

func setEnvIfPresent(v *viper.Viper, key, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		v.Set(key, value)
	}
}
