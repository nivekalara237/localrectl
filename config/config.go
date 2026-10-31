package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nivekalara237/localrectl/internal/build"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cast"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	AppHomeDir        = "$HOME/.lr"
	AppUser           = "local-registry"
	AppConfigFileName = AppHomeDir + "/config"
	envPrefix         = "LOCAL_REGISTRY"
)

var (
	configSearchDirs = []string{
		"/etc/" + build.GetBuildInfo().Name,
		AppHomeDir,
		".",
	}

	configFileName = []string{
		"config",
		"config.yaml",
		"config.yml",
	}
)

var nestedFlags = map[string]string{
	"server.url":          "server-url",
	"server.upload-token": "server-upload-token",
	"server.ca-file":      "server-ca-file",
}

// Keys are the settings, in the order 'lrctl config' prints them.
var Keys = []string{
	"server.url", "server.upload-token", "server.ca-file",
	"output", "retries", "timeout", "no-color", "verbose", "quiet",
	"exit-code",
}

// Config is the resolved configuration of one run. Each LoadConfig builds its own: nothing is
// kept from one run to the next.
type Config struct {
	AppUntypedConfig
	httpClient *httpclient.Client

	v    *viper.Viper
	fs   *pflag.FlagSet
	file string
}

type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// Usage marks err as a usage error: a bad flag, variable or config file, not a failure of the
// command itself.
func Usage(err error) error {
	if err == nil {
		return nil
	}
	return usageError{err}
}

// IsUsageError tells whether err, or an error it wraps, is a usage error.
func IsUsageError(err error) bool {
	var u usageError
	return errors.As(err, &u)
}

// findDefaultConfig returns the first config file of the search directories, and the
// directories it looked in.
func findDefaultConfig() (file string, searched []string) {
	for _, dir := range configSearchDirs {
		d, err := ExpandPath(dir)
		if err != nil {
			continue
		}
		searched = append(searched, d)
		for _, name := range configFileName {
			p := filepath.Join(d, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p, searched
			}
		}
	}
	return "", searched
}

// LoadConfig resolves and validates the settings: flags, then LOCAL_REGISTRY_* variables,
// then the --config file, then defaults. Any problem with them is a usageError. What it found
// is added to notes, to log once --verbose and --quiet are known.
//
// The Config is never nil: when LoadConfig fails, its OutputOptions still tell how to print
// the error.
func LoadConfig(fs *pflag.FlagSet, notes *logging.Notes) (*Config, error) {
	v := viper.New()
	c := &Config{v: v, fs: fs}

	// The defaults that no flag gives: the other ones are the flag defaults, shown by --help.
	v.SetDefault("server.url", "http://localhost:8077")
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(fs); err != nil { // only on a nil flag, which a FlagSet never holds
		return c, err
	}
	for k, name := range nestedFlags {
		if err := v.BindPFlag(k, fs.Lookup(name)); err != nil {
			return c, fmt.Errorf("--%s: %w", name, err)
		}
	}
	v.MustBindEnv("server.url", envPrefix+"_URL")
	v.MustBindEnv("server.upload-token", envPrefix+"_UPLOAD_TOKEN")
	v.MustBindEnv("server.ca-file", envPrefix+"_CA_FILE")

	explicitPath, err := ExpandPath(v.GetString("config"))
	if err != nil {
		return c, usagef("config: %v", err)
	}

	var file string
	if explicitPath != "" {
		file = explicitPath
		info, err := os.Stat(file)
		if err != nil {
			return c, usagef("config file %s: %v", file, err)
		}
		if info.IsDir() {
			return c, usagef("config file %s: is a directory", file)
		}
		notes.Debug("config file", "path", file, "source", c.Source("config"))
	} else {
		var searched []string
		file, searched = findDefaultConfig()
		if file != "" {
			notes.Debug("config file found", "path", file)
		} else {
			notes.Debug("no config file", "searched", strings.Join(searched, ", "))
		}
	}

	if file != "" {
		v.SetConfigFile(file)
		v.SetConfigType("yaml")
		if err := v.ReadInConfig(); err != nil {
			return c, usagef("config file %s: %v", file, err)
		}
		c.file = file

		known := fileKeys(fs)
		for _, k := range v.AllKeys() {
			if v.InConfig(k) && !slices.Contains(known, k) {
				return c, usagef("config file %s: unknown setting %q", file, k)
			}
		}
		if v.InConfig("server.upload-token") {
			if info, err := os.Stat(file); err == nil && info.Mode().Perm()&0o077 != 0 {
				notes.Warn("the config file holds the upload token but is readable by other users",
					"file", file, "mode", info.Mode().Perm().String(), "fix", "chmod 600 "+file)
			}
		}
	}

	// Typed reads that fail loudly: viper's plain getters turn "abc" into 0 without a word.
	var errs []error
	str := func(key string) string {
		s, err := cast.ToStringE(v.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", key, err))
		}
		return s
	}
	boolean := func(key string) bool {
		b, err := cast.ToBoolE(v.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", key, err))
		}
		return b
	}
	iint := func(key string) int {
		n, err := cast.ToIntE(v.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a whole number", key, v.Get(key)))
		}
		return n
	}
	// secret refuses a non-string value: YAML reads an unquoted token of digits as a number, and
	// a long one comes back as a float64 that lost digits. The value is never nil: viper skips a
	// null in the file, and the flag default is "".
	secret := func(key string) string {
		val := v.Get(key)
		s, ok := val.(string)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: read as a %T, not a string: quote it in the config file", key, val))
		}
		return s
	}
	// path also expands $VAR, ${VAR} and a leading ~: a value from the config file, a systemd
	// unit or a single-quoted argument never went through a shell.
	path := func(key string) string {
		p, err := ExpandPath(str(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", key, err))
		}
		return p
	}

	cfg := &c.AppUntypedConfig
	cfg.Server.BaseUrl = path("server.url")
	cfg.Server.UploadToken = secret("server.upload-token")
	cfg.Server.CaFile = path("server.ca-file")
	cfg.Output = str("output")
	cfg.NoColor = boolean("no-color")
	cfg.Verbose = boolean("verbose")
	cfg.Quiet = boolean("quiet")
	cfg.Retries = iint("retries")
	cfg.Timeout = iint("timeout")
	cfg.ExitCode = iint("exit-code")

	if len(errs) > 0 {
		return c, usageError{errors.Join(errs...)}
	}
	if err := validate(cfg); err != nil {
		return c, err
	}
	// The source only, never the value: one of them is the upload token.
	for _, k := range Keys {
		notes.Debug("setting", "key", k, "source", c.Source(k))
	}
	return c, nil
}

// File is the config file read, or "" when there was none.
func (c *Config) File() string {
	return c.file
}

// OutputOptions are the output settings as resolved so far. They also serve when LoadConfig
// failed, to print its error in the format asked for.
func (c *Config) OutputOptions() logging.Options {
	return logging.Options{
		Format:  logging.Format(c.v.GetString("output")),
		Verbose: c.v.GetBool("verbose"),
		Quiet:   c.v.GetBool("quiet"),
		NoColor: c.v.GetBool("no-color"),
	}
}

// Source tells where the value of key comes from: flag, env LOCAL_REGISTRY_<Upper(flagName)>, config file or
// default.
func (c *Config) Source(key string) string {
	name := key
	if n, ok := nestedFlags[key]; ok {
		name = n
	}
	if f := c.fs.Lookup(name); f != nil && f.Changed {
		return "flag"
	}

	env := envName(strings.TrimPrefix(key, "server."))
	if _, ok := os.LookupEnv(env); ok {
		return "env " + env
	}
	if c.v.InConfig(key) {
		return "config file"
	}

	return "default"
}

type configKey struct{}

// With stores c in ctx, for the commands to find it with From.
func With(ctx context.Context, c *Config) context.Context {
	return context.WithValue(ctx, configKey{}, c)
}

// From returns the Config stored by With, or nil.
func From(ctx context.Context) *Config {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(configKey{}).(*Config)
	return c
}

// validate checks the settings and completes the derived ones (absolute paths, audit log).
func validate(cfg *AppUntypedConfig) error {
	opts := logging.Options{Format: logging.Format(cfg.Output), Verbose: cfg.Verbose, Quiet: cfg.Quiet}
	if err := opts.Validate(); err != nil {
		return usageError{err}
	}
	f, _ := logging.ParseFormat(cfg.Output)
	cfg.Output = string(f)
	for _, l := range []struct {
		name string
		v    int
	}{{"retries", cfg.Retries}, {"timeout", cfg.Timeout}} {
		if l.v < 0 {
			return usagef("%s cannot be negative", l.name)
		}
	}
	// Above 125, the shells give the codes a meaning (126 not executable, 127 not found, 128+n
	// killed by signal n), and the system keeps only the low 8 bits.
	if cfg.ExitCode < 0 || cfg.ExitCode > 125 {
		return usagef("exit-code %d must be between 0 and 125", cfg.ExitCode)
	}

	if cfg.Server.BaseUrl != "" {
		ur, err := url.Parse(cfg.Server.BaseUrl)
		if err != nil || (ur.Scheme != "http" && ur.Scheme != "https") || ur.Host == "" ||
			ur.User != nil || ur.RawQuery != "" || ur.Fragment != "" || strings.HasSuffix(cfg.Server.BaseUrl, "?") {
			return usagef(
				"server.url %s must be an http or https URL with no user, query or fragment,"+
					" such as https://registry.example.com", cfg.Server.BaseUrl)
		}
		cfg.Server.BaseUrl = strings.TrimRight(cfg.Server.BaseUrl, "/")
	}
	return nil
}

func AddFlags(fs *pflag.FlagSet) {
	fs.String("config", "", "Yaml file configuration")
	fs.String("server-url", "", "Local Registry Server Base URL")
	fs.String("server-upload-token", "", "Local Registry Token for secure upload")
	fs.String("server-ca-file", "", "Local Registry CA file for internal HTTPS")

	fs.Bool("no-color", false, "Output without color")
	fs.Int("retries", 3, "News attempts against network 5xx error (3 by default)")
	fs.Int("timeout", 300, "Timeout delay(5min default)")
	fs.Int("exit-code", 1, "Exit code of a failed command, for the CI (0 never fails the job; usage errors stay 2)")

	fs.SortFlags = false
	fs.VisitAll(func(flag *pflag.Flag) {
		if slices.Contains([]string{
			"config",
			"server-url",
			"server-upload-token",
			"server-ca-file",
		}, flag.Name) {
			s := strings.TrimPrefix(flag.Name, "server-")
			flag.Usage += fmt.Sprintf(" - env[%s]", envName(s))
		}
	})
}

func envName(key string) string {
	return envPrefix + "_" + strings.ToUpper(
		strings.NewReplacer("-", "_", ".", "_").
			Replace(key),
	)
}

func fileKeys(fs *pflag.FlagSet) (keys []string) {
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name != "config" && f.Name != "help" {
			keys = append(keys, f.Name)
		}
	})
	for k := range nestedFlags {
		keys = append(keys, k)
	}
	return keys
}

// ExpandPath replaces $VAR and ${VAR} by their value, and a leading ~ by the home directory.
// An unset variable is an error, not an empty string: "$HOMEE/x" must not become "/x".
func ExpandPath(p string) (string, error) {
	var missing []string
	p = os.Expand(p, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, "$"+name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %v", err)
		}
		p = home + p[1:]
	} else if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("%s: only ~ and ~/ are expanded, not ~user", p)
	}
	return p, nil
}

func (c *Config) HttpClient() *httpclient.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	var opts []httpclient.Option
	opts = append(opts,
		httpclient.WithBaseURL(c.Server.BaseUrl),
		httpclient.WithCache(httpclient.NewInMemoryCache()),
		httpclient.WithAllowSubdomain(true),
	)
	if c.Timeout > 0 {
		opts = append(opts, httpclient.WithTimeout(time.Duration(c.Timeout)*time.Second))
	}
	if c.Retries > 0 {
		opts = append(opts, httpclient.WithRetry(
			c.Retries, 1*time.Second,
		))
	}
	if c.Server.CaFile != "" {
		opts = append(opts, httpclient.WithTLSCACertFile(c.Server.CaFile))
	}
	c.httpClient = httpclient.NewClientHttp(opts...)
	return c.httpClient
}
