package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/risk"
)

// ErrNothingToSnapshot: the requested services hold no data and depend on none that does.
var ErrNothingToSnapshot = errors.New("nothing to snapshot: the services hold no data and depend on no service that does")

// VolumePlan is one piece of data that will be archived.
type VolumePlan struct {
	Service  string `json:"service"`
	Type     string `json:"type"`             // volume | bind
	Name     string `json:"name,omitempty"`   // docker volume name
	Source   string `json:"source,omitempty"` // host path (bind)
	Target   string `json:"target"`
	External bool   `json:"external,omitempty"`
	Size     int64  `json:"size"` // -1 when unknown
}

// ProtectedPlan says how one data-holding service is snapshotted.
type ProtectedPlan struct {
	Name         string       `json:"name"`
	Kind         compose.Kind `json:"kind"`
	Image        string       `json:"image"`
	Dump         bool         `json:"dump"`    // logical dump (Postgres only)
	Archive      bool         `json:"archive"` // cold archive of the data
	RestoreCheck bool         `json:"restore_check"`
	PasswordEnv  string       `json:"-"`
	Volumes      []VolumePlan `json:"volumes"`
}

// SnapshotPlan is what a snapshot would do, shown by the dry run and by the
// confirmation prompt.
type SnapshotPlan struct {
	Project    string          `json:"project"`
	Requested  []string        `json:"requested"`
	Protected  []ProtectedPlan `json:"protected"`
	KeepImages []string        `json:"keep_images"` // services whose image gets a composesafe-keep tag
	ColdStop   []string        `json:"cold_stop"`   // stopped while their data is archived
	Quiesce    []string        `json:"quiesce"`     // writers stopped around the snapshot
	Mode       string          `json:"mode"`

	DataSize   int64    `json:"data_size"`   // -1 when unknown
	BackupNeed int64    `json:"backup_need"` // bytes needed where snapshots are written; -1 when unknown
	BackupFree int64    `json:"backup_free"` // -1 when unknown
	DockerNeed int64    `json:"docker_need"` // bytes needed in Docker's data area for the restore check
	Warnings   []string `json:"warnings,omitempty"`
}

func isPGKind(k compose.Kind) bool {
	return k == compose.KindPostgres || k == compose.KindPostgresDerivative
}

// dataMounts returns the read-write mounts that hold a service's data:
// for Postgres the mount under /var/lib/postgresql, otherwise every
// read-write volume or bind mount.
func dataMounts(s *compose.Service) []compose.Mount {
	var out []compose.Mount
	for _, m := range s.Mounts {
		if m.ReadOnly || (m.Type != "volume" && m.Type != "bind") {
			continue
		}
		if isPGKind(s.Kind) && !strings.HasPrefix(m.Target, "/var/lib/postgresql") {
			continue
		}
		out = append(out, m)
	}
	return out
}

// dependents returns every service that depends, directly or through others,
// on any service in set (excluding the set itself), sorted.
func dependents(p *compose.Project, set []string) []string {
	in := map[string]bool{}
	for _, n := range set {
		in[n] = true
	}
	found := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, s := range p.Services {
			if in[name] || found[name] {
				continue
			}
			for _, dep := range s.DependsOn {
				if in[dep] || found[dep] {
					found[name] = true
					changed = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// planSnapshot decides what a snapshot of the requested services involves. It
// is pure: sizes and free space are passed in.
func planSnapshot(p *compose.Project, cfg *config.Config, o SnapshotOptions, vols map[string]int64,
	dirSize func(string) int64, backupFree int64) (*SnapshotPlan, error) {

	if len(o.Services) == 0 {
		return nil, errors.New("name at least one service")
	}
	for _, n := range o.Services {
		if p.Services[n] == nil {
			return nil, fmt.Errorf("unknown service %q (services: %s)", n, strings.Join(p.ServiceNames(), ", "))
		}
	}

	prot := map[string]bool{}
	for _, n := range o.Services {
		for _, x := range protectedServices(p, cfg, n) {
			prot[x] = true
		}
	}
	if len(prot) == 0 && !o.AllowNoData {
		return nil, ErrNothingToSnapshot
	}

	plan := &SnapshotPlan{Project: p.Name, Requested: append([]string(nil), o.Services...), BackupFree: backupFree}
	keep := map[string]bool{}
	for _, n := range o.Services {
		keep[n] = true
	}
	for n := range prot {
		keep[n] = true
	}

	var names []string
	for n := range prot {
		names = append(names, n)
	}
	sort.Strings(names)

	var total int64
	unknown := false
	anyDump, anyArchive, anyCheck := false, false, false
	mode := ""
	for _, n := range names {
		s := p.Services[n]
		sc := cfg.Service(n)
		pp := ProtectedPlan{Name: n, Kind: s.Kind, Image: s.Image, PasswordEnv: sc.PGPasswordEnv}

		m := sc.Snapshot.Mode
		if o.LogicalOnly {
			m = "logical"
		}
		if isPGKind(s.Kind) {
			pp.Dump = strings.Contains(m, "logical")
			pp.Archive = strings.Contains(m, "volume")
		} else {
			// Only a cold archive is possible for anything but Postgres. Under
			// a dump-only mode there is nothing to do for such a service.
			pp.Archive = strings.Contains(m, "volume")
			if !pp.Archive {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("%s holds no Postgres data, so a dump-only snapshot skips it: its data is NOT backed up", n))
				continue
			}
		}
		pp.RestoreCheck = pp.Dump && !o.SkipRestoreCheck && *sc.Snapshot.RestoreCheck
		anyDump, anyArchive, anyCheck = anyDump || pp.Dump, anyArchive || pp.Archive, anyCheck || pp.RestoreCheck
		mode = joinMode(mode, pp)

		for _, mt := range dataMounts(s) {
			v := VolumePlan{Service: n, Type: mt.Type, Name: mt.VolumeName, Source: mt.Source, Target: mt.Target,
				External: mt.External, Size: -1}
			switch mt.Type {
			case "volume":
				if mt.Source == "" {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: anonymous volume at %s cannot be archived and is skipped", n, mt.Target))
					continue
				}
				if sz, ok := vols[mt.VolumeName]; ok {
					v.Size = sz
				}
				if mt.External {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: volume %s is external and may be shared with other projects", n, mt.VolumeName))
				}
			case "bind":
				v.Size = dirSize(mt.Source)
				if v.Size < 0 {
					return nil, fmt.Errorf("%s: bind mount source %s does not exist", n, mt.Source)
				}
			}
			if v.Size < 0 {
				unknown = true
			}
			total += max64(v.Size, 0)
			pp.Volumes = append(pp.Volumes, v)
		}
		if pp.Archive && len(pp.Volumes) == 0 {
			return nil, fmt.Errorf("%s: no data mount to archive (only named volumes and bind mounts are supported)", n)
		}
		plan.Protected = append(plan.Protected, pp)
		if pp.Archive {
			plan.ColdStop = append(plan.ColdStop, n)
		}
	}
	if len(plan.Protected) == 0 {
		if !o.AllowNoData {
			return nil, fmt.Errorf("%w (a dump-only snapshot covers Postgres only)", ErrNothingToSnapshot)
		}
		mode = "config"
	}
	plan.Mode = mode

	for n := range keep {
		if p.Services[n].Image != "" {
			plan.KeepImages = append(plan.KeepImages, n)
		}
	}
	sort.Strings(plan.KeepImages)

	// Writers: whatever depends on a service that is stopped for archiving,
	// plus any writers named in the config. Nothing is stopped for a pure
	// logical dump: pg_dumpall is a consistent snapshot of a live server.
	if len(plan.ColdStop) > 0 {
		q := map[string]bool{}
		for _, n := range dependents(p, plan.ColdStop) {
			q[n] = true
		}
		for _, n := range names {
			for _, w := range cfg.Service(n).Snapshot.Quiesce {
				if p.Services[w] == nil {
					return nil, fmt.Errorf("services.%s.snapshot.quiesce: unknown service %q", n, w)
				}
				q[w] = true
			}
		}
		for _, n := range plan.ColdStop {
			delete(q, n)
		}
		for n := range q {
			plan.Quiesce = append(plan.Quiesce, n)
		}
		sort.Strings(plan.Quiesce)
	}

	plan.DataSize = risk.Unknown
	plan.BackupNeed = risk.Unknown
	if !unknown {
		plan.DataSize = total
		factor := 1.1
		if anyDump && anyArchive {
			factor = 2.2
		}
		plan.BackupNeed = int64(float64(total) * factor)
		if anyCheck {
			plan.DockerNeed = total
		}
	}
	return plan, nil
}

func joinMode(cur string, pp ProtectedPlan) string {
	m := ""
	switch {
	case pp.Dump && pp.Archive:
		m = "logical+volume"
	case pp.Dump:
		m = "logical"
	default:
		m = "volume"
	}
	if cur == "" || cur == m {
		return m
	}
	return "mixed"
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
