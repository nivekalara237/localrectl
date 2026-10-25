package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/nivekalara237/localrectl/internal/build"
	"github.com/spf13/cast"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	AppHomeDir        = "$HOME/.local-registry"
	AppUser           = "local-registry"
	AppConfigFileName = AppHomeDir + "/config"
	envPrefix         = "LOCAL_REGISTRY"
)

var (
	Vconfig   = viper.New()
	AppConfig AppUntypedConfig
)

type OutputType = string

var (
	OText  OutputType = "text"
	OJson  OutputType = "json"
	OTable OutputType = "table"
)

type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args)}
}

func InitConfig() {
	Vconfig.SetConfigName("config")
	// Vconfig.SetConfigType("toml")
	Vconfig.AddConfigPath("/etc/" + build.GetBuildInfo().Name + "/")
	Vconfig.AddConfigPath(AppHomeDir)
	Vconfig.AddConfigPath(".")
	Vconfig.SetEnvPrefix(strings.ToUpper(AppUser))
	Vconfig.SetEnvKeyReplacer(
		strings.NewReplacer(".", "_"),
	)
	Vconfig.AutomaticEnv()
}

func LoadAppConfig() (AppUntypedConfig, error) {

	serverEnvs := []string{
		"url",
		"upload_token",
		"ca_file",
	}

	for _, s := range serverEnvs {
		replace := strings.ReplaceAll(s, ".", "_")
		_ = Vconfig.BindEnv(fmt.Sprintf("server.%s", s), fmt.Sprintf("%s_%s",
			strings.ToUpper(AppUser), strings.ToUpper(replace)))
	}

	var cfg AppUntypedConfig

	if err := Vconfig.Unmarshal(&cfg); err != nil {
		return cfg, usagef("decode configuration: maybe malformatted: %w", err)
	}

	AppConfig = cfg

	if strings.TrimSpace(cfg.Server.BaseUrl) == "" {
		return cfg, fmt.Errorf("invalid server.base-url: %s", cfg.Server.BaseUrl)
	}
	if strings.TrimSpace(cfg.Server.UploadToken) == "" {
		slog.With("config").Warn("server.upload-token is not set, so lrclt will read only")
	}

	return cfg, nil
}

func LoadConfigFile() error {
	cfgFile := Vconfig.GetString("config")

	if cfgFile != "" {
		Vconfig.SetConfigFile(cfgFile)
		if err := Vconfig.ReadInConfig(); err != nil {
			return fmt.Errorf("error reading config file: %w", err)
		}
		return nil
	}

	if err := Vconfig.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); ok {
			return nil
		}

		return fmt.Errorf("error reading config: %w", err)
	}

	return nil
}

func SetDefaults() {
	Vconfig.SetDefault("server.url", "http://localhost:8077")
	Vconfig.SetDefault("no-color", false)
	Vconfig.SetDefault("timeout", 300) // seconds
	Vconfig.SetDefault("retries", 3)
	Vconfig.SetDefault("verbose", false)
	Vconfig.SetDefault("output", OText)
}

// LoadConfig resolves and validates the settings: flags, then LOCAL_REGISTRY_* variables,
// then the --config file, then defaults. Any problem is a usageError.
func LoadConfig(fs *pflag.FlagSet, stderr io.Writer) error {
	Vconfig.SetConfigName("config")
	// Vconfig.SetConfigType("toml")
	Vconfig.AddConfigPath("/etc/" + build.GetBuildInfo().Name + "/")
	Vconfig.AddConfigPath(AppHomeDir)
	Vconfig.AddConfigPath(".")
	Vconfig.SetEnvPrefix(envPrefix)
	Vconfig.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	Vconfig.AutomaticEnv()
	if err := Vconfig.BindPFlags(fs); err != nil {
		return err
	}

	file, err := ExpandPath(Vconfig.GetString("config"))
	if err != nil {
		return usagef("config: %v", err)
	}
	if file != "" {
		Vconfig.SetConfigFile(file)
		Vconfig.SetConfigType("yaml")
		if err := Vconfig.ReadInConfig(); err != nil {
			return usagef("config file %s: %v", file, err)
		}
		known := fileKeys(fs)
		for _, k := range Vconfig.AllKeys() {
			if Vconfig.InConfig(k) && !slices.Contains(known, k) {
				return usagef("config file %s: unknown setting %q", file, k)
			}
		}
		if Vconfig.InConfig("server.upload-token") {
			if info, err := os.Stat(file); err == nil && info.Mode().Perm()&0o077 != 0 {
				_, _ = fmt.Fprintf(stderr, "warning: %s holds the upload token but is readable by other users (mode %v): chmod 600 %s\n",
					file, info.Mode().Perm(), file)
			}
		}
	}

	// Typed reads that fail loudly: viper's plain getters turn "abc" into 0 without a word.
	var errs []error
	str := func(key string) string {
		s, err := cast.ToStringE(Vconfig.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", key, err))
		}
		return s
	}
	boolean := func(key string) bool {
		s, err := cast.ToBoolE(Vconfig.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", key, err))
		}
		return s
	}
	iint := func(key string) int {
		n, err := cast.ToIntE(Vconfig.Get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a whole number", key, Vconfig.Get(key)))
		}
		return n
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
	AppConfig.Server.BaseUrl = path("server-url")
	AppConfig.Server.UploadToken = path("server-token")
	AppConfig.Server.CaFile = path("ca-file")
	AppConfig.Output = str("output")
	AppConfig.NoColor = boolean("no-color")
	AppConfig.Verbose = boolean("verbose")
	AppConfig.Retries = iint("retries")
	AppConfig.Timeout = iint("timeout")

	if len(errs) > 0 {
		return usageError{errors.Join(errs...)}
	}
	return validate(AppConfig)
}

// validate checks the settings and completes the derived ones (absolute paths, audit log).
func validate(cfg AppUntypedConfig) error {
	isValidUrl := func(rawURL string) bool {
		u, err := url.ParseRequestURI(rawURL)
		ors := err != nil ||
			u.Scheme != "http" && u.Scheme != "https" ||
			u.Hostname() == "" ||
			strings.ContainsAny(rawURL, " \t\r\n")
		if ors {
			return false
		}
		return true
	}

	if !isValidUrl(cfg.Server.BaseUrl) {
		return usagef("server-url must be valid url: e.g: https://local-registry.corp.org")
	}
	if !slices.Contains([]OutputType{OText, OJson}, cfg.Output) {
		return usagef("output must be one of 'text' and 'json'")
	}
	for _, l := range []struct {
		name string
		v    int
	}{{"retries", cfg.Retries}, {"timeout", cfg.Timeout}} {
		if l.v < 0 {
			return usagef("%s cannot be negative", l.name)
		}
	}

	if cfg.Server.BaseUrl != "" {
		u, err := url.Parse(cfg.Server.BaseUrl)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || strings.HasSuffix(cfg.Server.BaseUrl, "?") {
			return usagef(
				"public-url %q must be an http or https URL with no user, query or fragment,"+
					" such as https://registry.example.com", cfg.Server.BaseUrl)
		}
		cfg.Server.BaseUrl = strings.TrimRight(cfg.Server.BaseUrl, "/")
	}
	return nil
}

func Source(fs *pflag.FlagSet, key string) string {
	if f := fs.Lookup(key); f != nil && f.Changed {
		return "flag"
	}
	if _, ok := os.LookupEnv(envName(key)); ok {
		return "env " + envName(key)
	}
	if Vconfig.InConfig(key) {
		return "config file"
	}
	return "default"
}

func AddFlags(fs *pflag.FlagSet) {
	fs.String("config", "", "Yaml file configuration")
	fs.String("server.url", "", "Local Registry Server Base URL")
	fs.String("server.upload-token", "", "Local Registry Token for secure upload")
	fs.Bool("no-color", false, "Output without color")
	fs.Int("retries", 3, "News attempts against network 5xx error (3 by default)")
	fs.Int("timeout", 300, "Timeout delay(5min default)")

	fs.SortFlags = false
	fs.VisitAll(func(flag *pflag.Flag) {
		if slices.Contains([]string{
			"config",
			"server.url",
			"server.upload-token",
		}, flag.Name) {
			s := strings.Trim(flag.Name, "server.")
			flag.Usage += fmt.Sprintf("env[%s]", envName(s))
		}
	})
}

func envName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

func fileKeys(fs *pflag.FlagSet) (keys []string) {
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name != "config" && f.Name != "help" {
			keys = append(keys, f.Name)
		}
	})
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
