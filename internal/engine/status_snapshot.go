package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/release"
)

// StatusSnapshot is the machine-readable, best-effort view of one application.
// Complete says whether every configured status source was observed. Diverged
// only reports divergence that was positively observed; an unavailable source
// makes the snapshot incomplete rather than inventing a healthy or unhealthy
// state.
type StatusSnapshot struct {
	App             string            `json:"app"`
	Host            string            `json:"host"`
	CapturedAt      time.Time         `json:"captured_at"`
	RecordedRelease string            `json:"recorded_release,omitempty"`
	Roles           []StatusRole      `json:"roles"`
	Services        []StatusService   `json:"services"`
	Incomplete      *StatusIncomplete `json:"incomplete,omitempty"`
	Proxy           *StatusProxy      `json:"proxy,omitempty"`
	Diverged        bool              `json:"diverged"`
	Complete        bool              `json:"complete"`
	Warnings        []StatusWarning   `json:"warnings,omitempty"`
	Runner          buildinfo.Runner  `json:"runner"`
}

// StatusRole is one configured application role and its observed containers.
type StatusRole struct {
	Name            string            `json:"name"`
	Service         string            `json:"service"`
	Mode            string            `json:"mode"`
	DesiredReplicas int               `json:"desired_replicas"`
	Containers      []StatusContainer `json:"containers"`
	Diverged        bool              `json:"diverged"`
	Issues          []string          `json:"issues,omitempty"`
}

// StatusService is one configured service and its observed containers.
// Services converge independently, so their release labels are facts but
// are not compared with the application's recorded release.
type StatusService struct {
	Name       string            `json:"name"`
	Containers []StatusContainer `json:"containers"`
	Diverged   bool              `json:"diverged"`
	Issues     []string          `json:"issues,omitempty"`
}

// StatusContainer contains only non-secret Docker status facts.
type StatusContainer struct {
	ID      string `json:"id"`
	Release string `json:"release,omitempty"`
	Health  string `json:"health"`
}

// StatusIncomplete is the resumable portion of an unfinished deployment.
type StatusIncomplete struct {
	DeployID        string   `json:"deploy_id"`
	Epoch           int      `json:"epoch"`
	PreviousRelease string   `json:"previous_release,omitempty"`
	GateOpen        bool     `json:"gate_open"`
	Completed       []string `json:"completed"`
	Operator        string   `json:"operator,omitempty"`
	GitSHA          string   `json:"git_sha,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
}

// StatusProxy is the structured state of the host-managed proxy. It is absent
// when this application does not manage the shared host proxy.
type StatusProxy struct {
	Managed           bool                `json:"managed"`
	Container         *StatusContainer    `json:"container,omitempty"`
	LocalConfigHash   string              `json:"local_config_hash,omitempty"`
	AppliedConfigHash string              `json:"applied_config_hash,omitempty"`
	ConfigDiverged    bool                `json:"config_diverged"`
	RegisteredApps    []string            `json:"registered_apps"`
	Certificates      []StatusCertificate `json:"certificates"`
	Diverged          bool                `json:"diverged"`
	Complete          bool                `json:"complete"`
	Issues            []string            `json:"issues,omitempty"`
}

// StatusCertificate is the public, private-key-free portion of an ACME entry.
type StatusCertificate struct {
	Domain         string    `json:"domain"`
	NotAfter       time.Time `json:"not_after"`
	DaysRemaining  int       `json:"days_remaining"`
	RenewalOverdue bool      `json:"renewal_overdue"`
}

// StatusWarning describes an observation that could not be completed.
type StatusWarning struct {
	Component string `json:"component"`
	Message   string `json:"message"`
}

type statusSnapshotRead struct {
	component string
	run       func() error
	err       error
}

const (
	statusProxyContainerRead = iota
	statusProxyAppliedHashRead
	statusProxyAppsRead
	statusProxyCertificatesRead
	statusProxyLocalHashRead
	statusProxyReadCount
)

var statusProxyReadComponents = [statusProxyReadCount]string{
	"proxy.container",
	"proxy.applied_config",
	"proxy.registered_apps",
	"proxy.certificates",
	"proxy.local_config",
}

// StatusSnapshot observes the same host facts as Status without rendering or
// turning ordinary read failures into a total failure. All independent reads
// run in one concurrent wave. Cancellation is different: callers asked the
// operation to stop, so it is returned as an error instead of partial data.
func (e *Engine) StatusSnapshot(ctx context.Context) (StatusSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return StatusSnapshot{}, err
	}

	snapshot := StatusSnapshot{
		App:        e.Spec.Name,
		Host:       e.T.Host(),
		CapturedAt: e.Opts.Now().UTC(),
		Runner:     e.Opts.Runner,
		Roles:      make([]StatusRole, 0, len(e.Spec.ReleaseOrder())),
		Services:   make([]StatusService, 0, len(e.Spec.ServiceNames())),
		Complete:   true,
	}

	var (
		recorded  string
		byService map[string][]svcContainer
		inc       journal.Summary
		incFound  bool
		px        proxyRaw
	)

	reads := []statusSnapshotRead{
		{
			component: "current_release",
			run: func() (err error) {
				recorded, err = release.Current(ctx, e.T, e.names())
				return err
			},
		},
		{
			component: "containers",
			run: func() (err error) {
				byService, err = e.projectContainers(ctx)
				return err
			},
		},
		{
			component: "incomplete_deployment",
			run: func() error {
				s, err := e.FindIncomplete(ctx)
				if errors.Is(err, ErrNoIncomplete) {
					return nil
				}
				if err != nil {
					return err
				}
				inc, incFound = s, true
				return nil
			},
		},
	}

	proxyReadStart := len(reads)
	if e.Spec.Proxy.Managed {
		proxyReads := e.proxyReads(ctx, &px)
		for i, run := range proxyReads {
			component := fmt.Sprintf("proxy.read_%d", i)
			if i < len(statusProxyReadComponents) {
				component = statusProxyReadComponents[i]
			}
			reads = append(reads, statusSnapshotRead{component: component, run: run})
		}
	}

	// gather already provides the transport-safe concurrent wave used by Status.
	// Wrappers retain each component's error so partial observations can be named.
	fns := make([]func() error, 0, len(reads))
	for i := range reads {
		i := i
		fns = append(fns, func() error {
			reads[i].err = reads[i].run()
			return nil
		})
	}
	_ = gather(fns...)

	if err := statusSnapshotContextError(ctx, reads); err != nil {
		return StatusSnapshot{}, err
	}

	for _, read := range reads {
		if read.err == nil {
			continue
		}
		snapshot.Complete = false
		snapshot.Warnings = append(snapshot.Warnings, StatusWarning{
			Component: read.component,
			Message:   read.err.Error(),
		})
	}

	releaseComplete := reads[0].err == nil
	containersComplete := reads[1].err == nil
	snapshot.RecordedRelease = recorded
	for _, roleName := range e.Spec.ReleaseOrder() {
		role := e.Spec.Workloads[roleName]
		status := makeStatusRole(roleName, role, byService[roleName], recorded, releaseComplete, containersComplete)
		snapshot.Diverged = snapshot.Diverged || status.Diverged
		snapshot.Roles = append(snapshot.Roles, status)
	}
	for _, accessoryName := range e.Spec.ServiceNames() {
		status := makeStatusService(accessoryName, byService[accessoryName], containersComplete)
		snapshot.Diverged = snapshot.Diverged || status.Diverged
		snapshot.Services = append(snapshot.Services, status)
	}

	if reads[2].err == nil && incFound {
		snapshot.Incomplete = makeStatusIncomplete(inc)
		snapshot.Diverged = true
	}

	if e.Spec.Proxy.Managed {
		proxyComplete := make([]bool, len(reads)-proxyReadStart)
		for i := proxyReadStart; i < len(reads); i++ {
			proxyComplete[i-proxyReadStart] = reads[i].err == nil
		}
		proxyStatus, parseWarning := makeStatusProxy(px, proxyComplete, snapshot.CapturedAt)
		snapshot.Proxy = &proxyStatus
		snapshot.Diverged = snapshot.Diverged || proxyStatus.Diverged
		snapshot.Complete = snapshot.Complete && proxyStatus.Complete
		if parseWarning != nil {
			snapshot.Warnings = append(snapshot.Warnings, *parseWarning)
		}
	}

	return snapshot, nil
}

func statusSnapshotContextError(ctx context.Context, reads []statusSnapshotRead) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, read := range reads {
		if errors.Is(read.err, context.Canceled) || errors.Is(read.err, context.DeadlineExceeded) {
			return read.err
		}
	}
	return nil
}

func makeStatusRole(name string, role app.Workload, raw []svcContainer, recorded string, releaseComplete, containersComplete bool) StatusRole {
	status := StatusRole{
		Name:            name,
		Service:         name,
		Mode:            role.Mode(),
		DesiredReplicas: role.Count(),
		Containers:      makeStatusContainers(raw),
	}
	if !containersComplete {
		return status
	}
	if len(status.Containers) != status.DesiredReplicas {
		status.Issues = append(status.Issues, fmt.Sprintf("replica count is %d; want %d", len(status.Containers), status.DesiredReplicas))
	}
	for _, container := range status.Containers {
		if releaseComplete {
			switch {
			case recorded == "":
				status.Issues = append(status.Issues, fmt.Sprintf("container %s is running but no release is recorded", container.ID))
			case container.Release == "":
				status.Issues = append(status.Issues, fmt.Sprintf("container %s is not onebox-deployed", container.ID))
			case container.Release != strings.TrimSpace(recorded):
				status.Issues = append(status.Issues, fmt.Sprintf("container %s runs release %s; recorded release is %s", container.ID, container.Release, strings.TrimSpace(recorded)))
			}
		}
		if container.Health != "healthy" && container.Health != "none" {
			status.Issues = append(status.Issues, fmt.Sprintf("container %s health is %s", container.ID, container.Health))
		}
	}
	status.Diverged = len(status.Issues) > 0
	return status
}

func makeStatusService(name string, raw []svcContainer, containersComplete bool) StatusService {
	status := StatusService{Name: name, Containers: makeStatusContainers(raw)}
	if !containersComplete {
		return status
	}
	if len(status.Containers) == 0 {
		status.Issues = append(status.Issues, "not running")
	}
	for _, container := range status.Containers {
		if container.Health != "healthy" && container.Health != "none" {
			status.Issues = append(status.Issues, fmt.Sprintf("container %s is %s", container.ID, container.Health))
		}
	}
	status.Diverged = len(status.Issues) > 0
	return status
}

func makeStatusContainers(raw []svcContainer) []StatusContainer {
	containers := make([]StatusContainer, 0, len(raw))
	for _, container := range raw {
		containers = append(containers, StatusContainer{
			ID:      container.id,
			Release: container.release,
			Health:  container.health,
		})
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	return containers
}

func makeStatusIncomplete(summary journal.Summary) *StatusIncomplete {
	completed := make([]string, 0, len(summary.Done))
	for step, done := range summary.Done {
		if done {
			completed = append(completed, step)
		}
	}
	sort.Strings(completed)
	return &StatusIncomplete{
		DeployID:        summary.DeployID,
		Epoch:           summary.Epoch,
		PreviousRelease: summary.PrevRelease,
		GateOpen:        summary.GateOpen,
		Completed:       completed,
		Operator:        summary.Operator,
		GitSHA:          summary.GitSHA,
		StartedAt:       summary.StartedAt,
	}
}

func makeStatusProxy(raw proxyRaw, readComplete []bool, now time.Time) (StatusProxy, *StatusWarning) {
	complete := func(index int) bool { return index < len(readComplete) && readComplete[index] }
	status := StatusProxy{
		Managed:        true,
		RegisteredApps: []string{},
		Certificates:   []StatusCertificate{},
		Complete:       len(readComplete) == statusProxyReadCount,
	}
	for i := 0; i < statusProxyReadCount; i++ {
		status.Complete = status.Complete && complete(i)
	}

	if complete(statusProxyContainerRead) {
		if len(raw.ids) == 0 {
			status.Issues = append(status.Issues, "not running")
		} else {
			status.Container = &StatusContainer{ID: raw.ids[0], Health: raw.health}
			if raw.health != "healthy" {
				status.Issues = append(status.Issues, fmt.Sprintf("container health is %s", raw.health))
			}
		}
	}

	if complete(statusProxyLocalHashRead) {
		status.LocalConfigHash = raw.localHash
	}
	if complete(statusProxyAppliedHashRead) {
		status.AppliedConfigHash = raw.applied
	}
	if complete(statusProxyLocalHashRead) && complete(statusProxyAppliedHashRead) && raw.localHash != raw.applied {
		status.ConfigDiverged = true
		status.Issues = append(status.Issues, "local and applied configuration hashes differ")
	}

	if complete(statusProxyAppsRead) {
		status.RegisteredApps = strings.Fields(raw.apps)
		sort.Strings(status.RegisteredApps)
	}

	var parseWarning *StatusWarning
	if complete(statusProxyCertificatesRead) {
		certs, err := proxy.CertExpiries([]byte(raw.acme))
		if err != nil {
			status.Complete = false
			status.Issues = append(status.Issues, "certificate store is unreadable")
			parseWarning = &StatusWarning{Component: "proxy.certificates", Message: err.Error()}
		} else {
			for _, cert := range certs {
				days := int(cert.NotAfter.Sub(now).Hours() / 24)
				overdue := days < renewalFloorDays
				status.Certificates = append(status.Certificates, StatusCertificate{
					Domain:         cert.Domain,
					NotAfter:       cert.NotAfter.UTC(),
					DaysRemaining:  days,
					RenewalOverdue: overdue,
				})
				if overdue {
					status.Issues = append(status.Issues, fmt.Sprintf("certificate renewal is overdue for %s", cert.Domain))
				}
			}
			sort.Slice(status.Certificates, func(i, j int) bool {
				if status.Certificates[i].Domain == status.Certificates[j].Domain {
					return status.Certificates[i].NotAfter.Before(status.Certificates[j].NotAfter)
				}
				return status.Certificates[i].Domain < status.Certificates[j].Domain
			})
		}
	}

	status.Diverged = len(status.Issues) > 0
	return status, parseWarning
}
