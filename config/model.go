package config

type AppUntypedConfig struct {
	Server  ServerConf `json:"server" yaml:"server"`
	Timeout int        `json:"timeout" yaml:"timeout"`
	Retries int        `json:"retries" yaml:"retries"`
	Verbose bool       `json:"verbose" yaml:"verbose"`
	NoColor bool       `json:"no-color" yaml:"no-color"`
	Output  string     `json:"output" yaml:"output"`
	Quiet   bool       `json:"quiet" yaml:"quiet"`
}

type ServerConf struct {
	BaseUrl     string `json:"url" yaml:"url"`
	UploadToken string `json:"upload_token" yaml:"upload_token"`
	CaFile      string `json:"ca_file" yaml:"ca_file"`
}
