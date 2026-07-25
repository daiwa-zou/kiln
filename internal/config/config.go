package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Role distinguishes the two server roles, which differ in how they use
// Postgres and therefore in how their connection pools are sized.
type Role string

const (
	RoleServer Role = "server"
	RoleWorker Role = "worker"
)

// Config is the fully resolved configuration. Nothing downstream re-merges:
// Load returns this ready to use.
type Config struct {
	Role      Role   `mapstructure:"-"`
	HTTPAddr  string `mapstructure:"http_addr"`
	PublicURL string `mapstructure:"public_url"`
	LogLevel  string `mapstructure:"log_level"`

	Database Database `mapstructure:"database"`
	Storage  Storage  `mapstructure:"storage"`
	Agent    Agent    `mapstructure:"agent"`
	Worker   Worker   `mapstructure:"worker"`
	Secrets  Secrets  `mapstructure:"-"`
}

// Database holds Postgres connection and pool settings.
//
// URL wins when set; otherwise the discrete fields are assembled into a DSN, so
// Kubernetes deployments can source the password from a separate secret without
// templating a whole connection string.
type Database struct {
	URL         string `mapstructure:"url"`
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	Name        string `mapstructure:"name"`
	User        string `mapstructure:"user"`
	Password    string `mapstructure:"password"`
	SSLMode     string `mapstructure:"sslmode"`
	SSLRootCert string `mapstructure:"sslrootcert"`

	MaxConns        int32         `mapstructure:"max_conns"`
	MinConns        int32         `mapstructure:"min_conns"`
	MaxConnLifetime time.Duration `mapstructure:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `mapstructure:"max_conn_idle_time"`
}

// StorageBackend selects where blobs live.
type StorageBackend string

const (
	// BackendS3 targets any S3-compatible service: MinIO, AWS S3, R2, Backblaze.
	BackendS3 StorageBackend = "s3"
	// BackendFS writes to a mounted volume. Included so a single-node deployment
	// needs no object store at all; unsuitable once workers span hosts.
	BackendFS StorageBackend = "fs"
)

// Storage holds object storage settings.
type Storage struct {
	Backend   StorageBackend `mapstructure:"backend"`
	Endpoint  string         `mapstructure:"endpoint"`
	Region    string         `mapstructure:"region"`
	Bucket    string         `mapstructure:"bucket"`
	AccessKey string         `mapstructure:"access_key"`
	SecretKey string         `mapstructure:"secret_key"`
	UseSSL    bool           `mapstructure:"use_ssl"`
	// PathStyle is required by MinIO; AWS uses virtual-host addressing.
	PathStyle bool   `mapstructure:"path_style"`
	Path      string `mapstructure:"path"`

	RunArtifactRetention time.Duration `mapstructure:"run_artifact_retention"`
	SoftDeleteRetention  time.Duration `mapstructure:"soft_delete_retention"`
}

// Agent holds settings for the sandboxed claude invocations.
type Agent struct {
	Binary        string        `mapstructure:"binary"`
	Model         string        `mapstructure:"model"`
	AnalyzeModel  string        `mapstructure:"analyze_model"`
	FallbackModel string        `mapstructure:"fallback_model"`
	Timeout       time.Duration `mapstructure:"timeout"`

	AnalyzeBudgetUSD float64 `mapstructure:"analyze_budget_usd"`
	PageBudgetUSD    float64 `mapstructure:"page_budget_usd"`
	RunBudgetUSD     float64 `mapstructure:"run_budget_usd"`
	MaxPagesPerRun   int     `mapstructure:"max_pages_per_run"`
	// WarnTurns triggers a log warning; the installed claude CLI has no
	// --max-turns, so wall-clock capping is done with Timeout instead.
	WarnTurns int `mapstructure:"warn_turns"`
}

// Worker holds job execution settings.
type Worker struct {
	Concurrency   int           `mapstructure:"concurrency"`
	SweepInterval time.Duration `mapstructure:"sweep_interval"`
	SweepJitter   time.Duration `mapstructure:"sweep_jitter"`
}

// Secrets holds values that must never be logged or serialized. They are kept
// off the main structs so an accidental %+v on Config cannot leak them.
type Secrets struct {
	MasterKey           string
	AnthropicAPIKey     string
	SessionSecret       string
	GitHubPrivateKey    string
	GitHubWebhookSecret string
	StorageSecretKey    string
}

// DSN returns the Postgres connection string, preferring an explicitly
// configured URL and otherwise assembling one from the discrete fields.
func (d Database) DSN() string {
	if strings.TrimSpace(d.URL) != "" {
		return d.URL
	}

	q := url.Values{}
	if d.SSLMode != "" {
		q.Set("sslmode", d.SSLMode)
	}
	if d.SSLRootCert != "" {
		q.Set("sslrootcert", d.SSLRootCert)
	}

	u := &url.URL{
		Scheme:   "postgres",
		Host:     fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:     "/" + d.Name,
		RawQuery: q.Encode(),
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	return u.String()
}

// Redacted returns the DSN with any password replaced, for logging.
func (d Database) Redacted() string {
	u, err := url.Parse(d.DSN())
	if err != nil {
		return "postgres://<unparseable>"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}
