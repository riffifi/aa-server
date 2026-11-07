// Package buildinfo exposes only public release metadata, without host details.
package buildinfo

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

// Version and Commit can be set at build time with -ldflags -X.
var Version string
var Commit = "unknown"
var Dirty = "false"

//go:embed VERSION
var releaseVersion string

type Info struct {
	Service  string `json:"service"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Dirty    bool   `json:"dirty"`
	Protocol int    `json:"protocol"`
}

func Current(service string) Info {
	version := Version
	if version == "" {
		version = strings.TrimSpace(releaseVersion)
	}
	info := Info{Service: service, Version: version, Commit: Commit, Dirty: Dirty == "true", Protocol: 2}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				info.Dirty = info.Dirty || setting.Value == "true"
			}
		}
	}
	// Docker builds omit .git; the installer supplies a describe string instead.
	if len(info.Version) >= 6 && info.Version[len(info.Version)-6:] == "-dirty" {
		info.Dirty = true
	}
	return info
}
