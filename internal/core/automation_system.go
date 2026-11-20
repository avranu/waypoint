package core

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// System checks for admin automations (WP-D18): free disk, database size
// and free memory, sampled every minute against limits set in Admin →
// Automations. A resource that crosses into "low" fires system.resources_low
// once, and not again within the hour however it flaps.

// ResourceLimits are the thresholds; zero turns a check off.
type ResourceLimits struct {
	DiskFreeMB   int `json:"disk_free_mb"`   // free space on the data disk
	DatabaseMB   int `json:"database_mb"`    // size of the database file
	MemoryFreeMB int `json:"memory_free_mb"` // available memory
}

var defaultResourceLimits = ResourceLimits{DiskFreeMB: 1024, DatabaseMB: 0, MemoryFreeMB: 256}

// ResourceStatus is a reading and its limit.
type ResourceStatus struct {
	Resource string `json:"resource"` // disk, database, memory
	Label    string `json:"label"`
	Bytes    int64  `json:"bytes"`           // free (disk, memory) or used (database)
	Total    int64  `json:"total,omitempty"` // size of the disk or memory
	LimitMB  int    `json:"limit_mb"`
	Low      bool   `json:"low"`
}

// ResourceLimits returns the limits (admins only).
func (s *Service) GetResourceLimits(ctx context.Context, actor *Actor) (ResourceLimits, error) {
	if !actor.isAdmin() {
		return ResourceLimits{}, forbidden("system limits are for admins")
	}
	return s.resourceLimits(ctx), nil
}

func (s *Service) resourceLimits(ctx context.Context) ResourceLimits {
	lim := defaultResourceLimits
	var v string
	if err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = 'resource_limits'`).Scan(&v); err == nil {
		_ = json.Unmarshal([]byte(v), &lim)
	}
	return lim
}

// SetResourceLimits saves the limits (admins, people only).
func (s *Service) SetResourceLimits(ctx context.Context, actor *Actor, lim ResourceLimits) (ResourceLimits, error) {
	if err := actor.canWrite(); err != nil {
		return lim, err
	}
	if err := s.requireAdminHuman(actor); err != nil {
		return lim, err
	}
	for _, n := range []int{lim.DiskFreeMB, lim.DatabaseMB, lim.MemoryFreeMB} {
		if n < 0 || n > 100_000_000 {
			return lim, invalid("limits are megabytes, zero to turn one off")
		}
	}
	b, _ := json.Marshal(lim)
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES ('resource_limits', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(b)); err != nil {
		return lim, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "system.limits", Category: AuditSettings, TargetType: "settings", Target: "System limits", Detail: "changed the low-resource limits"})
	return lim, nil
}

// ResourceStatuses reads the resources now.
func (s *Service) ResourceStatuses(ctx context.Context, actor *Actor) ([]ResourceStatus, error) {
	if !actor.isAdmin() {
		return nil, forbidden("system status is for admins")
	}
	return s.readResources(ctx), nil
}

func (s *Service) readResources(ctx context.Context) []ResourceStatus {
	lim := s.resourceLimits(ctx)
	var out []ResourceStatus
	if free, total, err := diskSpace(filepath.Dir(s.db.Path())); err == nil {
		out = append(out, ResourceStatus{Resource: "disk", Label: "Free disk space", Bytes: free, Total: total, LimitMB: lim.DiskFreeMB,
			Low: lim.DiskFreeMB > 0 && free < int64(lim.DiskFreeMB)<<20})
	}
	if fi, err := os.Stat(s.db.Path()); err == nil {
		out = append(out, ResourceStatus{Resource: "database", Label: "Database size", Bytes: fi.Size(), LimitMB: lim.DatabaseMB,
			Low: lim.DatabaseMB > 0 && fi.Size() > int64(lim.DatabaseMB)<<20})
	}
	if free, total, ok := memoryAvailable(); ok {
		out = append(out, ResourceStatus{Resource: "memory", Label: "Free memory", Bytes: free, Total: total, LimitMB: lim.MemoryFreeMB,
			Low: lim.MemoryFreeMB > 0 && free < int64(lim.MemoryFreeMB)<<20})
	}
	return out
}

// memoryAvailable reads MemAvailable from /proc/meminfo, where there is one.
func memoryAvailable() (free, total int64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	var gotFree, gotTotal bool
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemAvailable":
			free, gotFree = kb<<10, true
		case "MemTotal":
			total, gotTotal = kb<<10, true
		}
	}
	return free, total, gotFree && gotTotal
}

// SampleResources looks at the resources once and fires the admin
// automations for any that just ran low.
func (s *Service) SampleResources(ctx context.Context) {
	for _, r := range s.readResources(ctx) {
		s.noteResource(ctx, r)
	}
}

func (s *Service) noteResource(ctx context.Context, r ResourceStatus) {
	var wasLow bool
	var last int64
	err := s.db.R.QueryRowContext(ctx, `SELECT low, last_fired_at FROM system_resource_state WHERE resource = ?`, r.Resource).Scan(&wasLow, &last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.WarnContext(ctx, "resource state", "err", err)
		return
	}
	now := s.nowMS()
	fire := r.Low && !wasLow && now-last >= time.Hour.Milliseconds()
	if fire {
		last = now
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO system_resource_state(resource, low, last_fired_at) VALUES (?,?,?)
		ON CONFLICT(resource) DO UPDATE SET low = excluded.low, last_fired_at = excluded.last_fired_at`, r.Resource, boolInt(r.Low), last); err != nil {
		s.log.WarnContext(ctx, "resource state", "err", err)
		return
	}
	if !fire {
		return
	}
	text := fmt.Sprintf("%s is low: %s", r.Label, humanBytes(r.Bytes))
	if r.Resource == "database" {
		text = fmt.Sprintf("The database has grown to %s (the limit is %d MB)", humanBytes(r.Bytes), r.LimitMB)
	}
	s.runGlobal(ctx, &Event{Kind: EvSystemResources, At: s.now().UTC(),
		Data: map[string]any{"resource": r.Resource, "free": r.Bytes, "total": r.Total, "limit_mb": r.LimitMB, "text": text}})
}

// runGlobal runs the admin automations an event triggers, for events that
// belong to no project (the system's).
func (s *Service) runGlobal(ctx context.Context, e *Event) {
	list, err := s.adminAutomations(ctx, true)
	if err != nil {
		s.log.ErrorContext(ctx, "load automations", "err", err)
		return
	}
	for _, a := range list {
		if a.Rule.Trigger.matches(e) {
			s.runAutomation(ctx, SystemActor(), a, e)
		}
	}
}

// BackupFailed tells admin automations the daily backup failed.
func (s *Service) backupFailed(ctx context.Context, err error) {
	s.runGlobal(ctx, &Event{Kind: EvBackupFailed, At: s.now().UTC(), Data: map[string]any{"error": err.Error()}})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
