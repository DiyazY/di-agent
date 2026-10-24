package collectors

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/internal/scripted"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/spec"
	"go.yaml.in/yaml/v3"
)

func init() {
	Register("cgroup", newCgroup)
	Register("netdata", newNetdata)
	Register("script", newScript)
}

type cgroupConfig struct {
	Root        string   `yaml:"root"`
	Subjects    bool     `yaml:"subjects"`
	Units       []string `yaml:"units"`
	MaxSubjects int      `yaml:"max_subjects"`
	CmdLabel    bool     `yaml:"cmd_label"`
	PodLogs     string   `yaml:"pod_logs"`
}

// defaultCgroupConfig is what a cgroup entry runs with when its config names
// nothing: the host's unified hierarchy, pods and allowlisted units as subjects.
func defaultCgroupConfig() cgroupConfig {
	return cgroupConfig{Root: "/sys/fs/cgroup", Subjects: true, Units: []string{}, MaxSubjects: 256, PodLogs: minimal.DefaultPodLogRoot}
}

// CgroupRoot is the cgroup hierarchy the agent reads its own cgroups from: the
// root of the first cgroup entry, so the agent's groups walk the same tree its
// collector reports on, or the default hierarchy when no entry is a cgroup one.
// An entry whose config does not decode yields the default too; Build has already
// refused that list.
func CgroupRoot(entries []spec.CollectorEntry) string {
	cfg := defaultCgroupConfig()
	for _, e := range entries {
		if e.Type != "cgroup" {
			continue
		}
		if err := decodeStrict(&e.Config, &cfg); err != nil {
			return defaultCgroupConfig().Root
		}
		break
	}
	return cfg.Root
}

func newCgroup(env Env, n *yaml.Node) (contracts.CollectorContract, error) {
	cfg := defaultCgroupConfig()
	if err := decodeStrict(n, &cfg); err != nil {
		return nil, err
	}
	if err := minimal.ValidUnitGlobs(cfg.Units); err != nil {
		return nil, err
	}
	return minimal.NewCgroupCollectorWithOptions(env.NodeID, cfg.Root, minimal.CgroupOptions{
		Subjects: cfg.Subjects, UnitGlobs: cfg.Units, MaxSubjects: cfg.MaxSubjects, CmdLabel: cfg.CmdLabel,
		PodLogRoot: cfg.PodLogs,
		// The kubelet links each container's log under /var/log/containers, beside the
		// per-pod /var/log/pods.
		ContainerLogRoot: containerLogRoot(cfg.PodLogs),
		Requests:         env.Requests,
	}), nil
}

// containerLogRoot is the kubelet's per-container link directory beside its per-pod one.
func containerLogRoot(podLogs string) string {
	if podLogs == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(podLogs)), "containers")
}

type netdataConfig struct {
	URL      string   `yaml:"url"`
	Include  []string `yaml:"include"`
	Collapse bool     `yaml:"collapse"`
}

func newNetdata(env Env, n *yaml.Node) (contracts.CollectorContract, error) {
	cfg := netdataConfig{Include: minimal.DefaultNetdataInclude, Collapse: true}
	if err := decodeStrict(n, &cfg); err != nil {
		return nil, err
	}
	if cfg.URL == "" {
		return nil, errors.New("config.url is required")
	}
	if err := minimal.ValidNetdataInclude(cfg.Include); err != nil {
		return nil, err
	}
	return minimal.NewNetdataCollectorWithOptions(env.NodeID, cfg.URL,
		minimal.NetdataOptions{Include: cfg.Include, Collapse: cfg.Collapse, Logf: env.Logf}), nil
}

type scriptConfig struct {
	Path string `yaml:"path"`
}

func newScript(env Env, n *yaml.Node) (contracts.CollectorContract, error) {
	var cfg scriptConfig
	if err := decodeStrict(n, &cfg); err != nil {
		return nil, err
	}
	if cfg.Path == "" {
		return nil, errors.New("config.path is required")
	}
	sc, err := scripted.LoadScenario(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("script %s: %w", cfg.Path, err)
	}
	// The script stamps simulated time one tick per Collect while the map sweeps
	// on the wall clock. An interval above the tick makes every property go stale
	// and then retire while the script is still emitting; one below it means
	// nothing ever goes stale. Not a drift to warn about: a configuration the
	// daemon must not start with.
	if tick := time.Duration(sc.TickSeconds) * time.Second; env.Interval > 0 && env.Interval != tick {
		return nil, fmt.Errorf("script %s ticks every %s but sources.interval is %s: simulated time would run "+
			"off the wall clock the map sweeps on, retiring or immortalising every property; set sources.interval: %s",
			sc.Name, tick, env.Interval, tick)
	}
	return scripted.NewSystemScript(env.NodeID, sc, time.Now())
}
