package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	App          AppConfig          `mapstructure:"app"`
	HTTP         HTTPConfig         `mapstructure:"http"`
	GRPC         GRPCConfig         `mapstructure:"grpc"`
	Database     DatabaseConfig     `mapstructure:"database"`
	Redis        RedisConfig        `mapstructure:"redis"`
	Auth         AuthConfig         `mapstructure:"auth"`
	FileSystem   FileSystemConfig   `mapstructure:"filesystem"`
	SolarNetwork SolarNetworkConfig `mapstructure:"solarNetwork"`
	Sentry       SentryConfig       `mapstructure:"sentry"`
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

type SolarNetworkConfig struct {
	BaseURL     string `mapstructure:"baseUrl"`
	AccessToken string `mapstructure:"accessToken"`
	AccountName string `mapstructure:"accountName"`
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
	v.SetDefault("solarNetwork.baseUrl", "")
	v.SetDefault("solarNetwork.accessToken", "")
	v.SetDefault("solarNetwork.accountName", "")
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
	setEnvIfPresent(v, "solarNetwork.baseUrl", "SOLAR_NETWORK_BASE_URL")
	setEnvIfPresent(v, "solarNetwork.accessToken", "SOLAR_NETWORK_ACCESS_TOKEN")
}

func setEnvIfPresent(v *viper.Viper, key, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		v.Set(key, value)
	}
}
