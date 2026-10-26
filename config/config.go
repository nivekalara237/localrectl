package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nivekalara237/localrectl/internal/build"
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
	Vconfig   = viper.New()
	AppConfig AppUntypedConfig
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

func findDefaultConfig() string {
	for _, dir := range configSearchDirs {
		for _, name := range configFileName {
			p := filepath.Join(dir, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p
			}
		}
	}
	return ""
}

func InitConfig() {
	// Vconfig.SetConfigName("config")
	//// Vconfig.SetConfigType("toml")
	// Vconfig.AddConfigPath("/etc/" + build.GetBuildInfo().Name + "/")
	/// Vconfig.AddConfigPath(AppHomeDir)
	// Vconfig.AddConfigPath(".")
	Vconfig.SetEnvPrefix(envPrefix)
	Vconfig.SetEnvKeyReplacer(
		strings.NewReplacer(".", "_"),
	)
	Vconfig.AutomaticEnv()
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
	// Vconfig.SetConfigName("config")
	//// Vconfig.SetConfigType("toml")
	// Vconfig.AddConfigPath("/etc/" + build.GetBuildInfo().Name + "/")
	/// Vconfig.AddConfigPath(AppHomeDir)
	// Vconfig.AddConfigPath(".")
	Vconfig.SetEnvPrefix(envPrefix)
	Vconfig.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	Vconfig.AutomaticEnv()
	if err := Vconfig.BindPFlags(fs); err != nil {
		return err
	}

	Vconfig.MustBindEnv("server.url", fmt.Sprintf("%s_%s", envPrefix, "URL"))
	Vconfig.MustBindEnv("server.upload-token", fmt.Sprintf("%s_%s", envPrefix, "UPLOAD_TOKEN"))
	Vconfig.MustBindEnv("server.ca-file", fmt.Sprintf("%s_%s", envPrefix, "CA_FILE"))

	explicitePath, err := ExpandPath(Vconfig.GetString("config"))
	if err != nil {
		return usagef("config: %v", err)
	}

	var file string

	if explicitePath != "" {
		file = explicitePath
		info, err := os.Stat(file)
		if err != nil {
			return usagef("config file %s: %v", file, err)
		}

		if info.IsDir() {
			return usagef("config file %s: is a directory", file)
		}
	} else {
		file = findDefaultConfig()
	}

	if file != "" {
		Vconfig.SetConfigFile(file)
		Vconfig.SetConfigType("yaml")
		if err := Vconfig.ReadInConfig(); err != nil {
			return usagef("config file %s: %v", file, err)
		}

		if used := Vconfig.ConfigFileUsed(); used != "" {
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
	}

	/*if file != "" {
		Vconfig.SetConfigFile(file)
		Vconfig.SetConfigType("yaml")
		if err := Vconfig.ReadInConfig(); err != nil {
			return usagef("config file %s: %v", file, err)
		}

	}*/

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

	fmt.Println(":::::::::::::::::::::::::::::::::::")
	fmt.Println(path("server.upload-token"))
	fmt.Println(path("server-upload-token"))
	fmt.Println(":::::::::::::::::::::::::::::::::::")

	AppConfig.Server.BaseUrl = path("server.url")
	AppConfig.Server.UploadToken = path("server.upload-token")
	AppConfig.Server.CaFile = path("server.ca-file")
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
	fmt.Println(cfg)
	if strings.TrimSpace(cfg.Output) != "" && !slices.Contains([]OutputType{OText, OJson}, cfg.Output) {
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
				"server.url %s must be an http or https URL with no user, query or fragment,"+
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

	if _, ok := os.LookupEnv(envName(strings.TrimPrefix(key, "server."))); ok {
		return "env " + envName(strings.TrimPrefix(key, "server."))
	}
	if Vconfig.InConfig(key) {
		return "config file"
	}

	return "default"
}

func AddFlags(fs *pflag.FlagSet) {
	fs.String("config", "", "Yaml file configuration")
	// fs.String("server.url", "", "Local Registry Server Base URL")
	// fs.String("server.upload-token", "", "Local Registry Token for secure upload")
	// fs.String("server.ca-file", "", "Local Registry CA file for internal HTTPS")
	fs.String("server-url", "", "Local Registry Server Base URL")
	fs.String("server-upload-token", "", "Local Registry Token for secure upload")
	fs.String("server-ca-file", "", "Local Registry CA file for internal HTTPS")

	fs.Bool("no-color", false, "Output without color")
	fs.Int("retries", 3, "News attempts against network 5xx error (3 by default)")
	fs.Int("timeout", 300, "Timeout delay(5min default)")

	for k, v := range nestedFlags {
		if err := Vconfig.BindPFlag(k, fs.Lookup(v)); err != nil {
			panic(err)
		}
		fs.Lookup(v)
	}

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
