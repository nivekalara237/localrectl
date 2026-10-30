package build

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "unknown"
	Date    = "unknown"
	Commit  = "unknown"
)

type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Dirty    bool   `json:"dirty"`
	GoVer    string `json:"go_version"`
	Platform string `json:"platform"`
	Name     string `json:"name"`
}

func GetBuildInfo() Info {
	i := Info{
		Version:  Version,
		Commit:   Commit,
		Date:     Date,
		GoVer:    runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Name:     "lrctl",
	}

	buildInfo, yes := debug.ReadBuildInfo()
	if !yes {
		return i
	}

	if i.Version == "dev" && buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
		i.Version = buildInfo.Main.Version
	}

	for _, s := range buildInfo.Settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "" && len(s.Value) >= 7 {
				i.Commit = s.Value[:7]
			}
		case "vcs.time":
			if i.Date == "" {
				i.Date = s.Value
			}
		case "vcs.modified":
			i.Dirty = s.Value == "true"
		}
	}

	return i
}

func (i Info) ToString() string {
	v := i.Version
	if i.Dirty {
		v += "-salle"
	}
	return fmt.Sprintf("%s %s (%s, built %s) %s %s", i.Name, v, i.Commit, i.Date, i.GoVer, i.Platform)
}
