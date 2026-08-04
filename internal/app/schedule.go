package app

import (
	"fmt"
	"strconv"
	"strings"
)

// A scheduled job is a promise that something runs at a time nobody will be
// watching. That makes it the worst place in the contract for an approximation:
// a nightly backup that silently never fires looks exactly like a nightly
// backup that works, right up until someone needs the backup.
//
// So the translation from the cron expression an author writes to the calendar
// expression the host understands is exact or it is refused. Every form this
// accepts is one whose meaning is identical on both sides; anything else fails
// at load with the reason, rather than being approximated into a schedule the
// author did not ask for.
//
// The host runs it. Onebox does not carry a scheduler, does not run a container
// whose job is to start other containers, and does not depend on its own
// process being alive at 2am. A timer on the box survives a reboot, and its
// last run is visible to anyone with a shell.

// ScheduledJob is a job workload that runs on a schedule.
type ScheduledJob struct {
	Name     string
	Cron     string
	Timezone string
	// Calendar is the host-side expression the cron translates to.
	Calendar string
}

// ScheduledJobs lists every job with a schedule, in a stable order.
func (p *Spec) ScheduledJobs() ([]ScheduledJob, error) {
	var out []ScheduledJob
	for _, name := range sortedKeys(p.Workloads) {
		w := p.Workloads[name]
		if !w.IsJob() || w.Schedule == nil {
			continue
		}
		cal, err := CronToCalendar(w.Schedule.Cron)
		if err != nil {
			return nil, errf("schedule_untranslatable", "workloads."+name+".schedule.cron", "",
				"%v", err)
		}
		tz := w.Schedule.Timezone
		if tz == "" {
			tz = "UTC"
		}
		out = append(out, ScheduledJob{Name: name, Cron: w.Schedule.Cron, Timezone: tz, Calendar: cal})
	}
	return out, nil
}

// CronToCalendar translates a five-field cron expression into a systemd
// calendar expression with identical meaning.
//
// It accepts `*`, a number, a list, a range, and a step over either. It refuses
// a day-of-month and a day-of-week together, because cron treats that pair as
// "either matches" and a calendar expression cannot say that — approximating it
// would move the job to days the author did not choose.
func CronToCalendar(expr string) (string, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", fmt.Errorf("a cron expression has five fields (minute hour day month weekday), got %d in %q",
			len(fields), expr)
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	if dom != "*" && dow != "*" {
		return "", fmt.Errorf(
			"%q sets both a day of month and a day of week; cron runs the job when EITHER matches, "+
				"which a host calendar cannot express — declare two jobs, one for each", expr)
	}

	min, err := translateField(minute, 0, 59, nil)
	if err != nil {
		return "", fmt.Errorf("minute: %w", err)
	}
	hr, err := translateField(hour, 0, 23, nil)
	if err != nil {
		return "", fmt.Errorf("hour: %w", err)
	}
	day, err := translateField(dom, 1, 31, nil)
	if err != nil {
		return "", fmt.Errorf("day of month: %w", err)
	}
	mon, err := translateField(month, 1, 12, nil)
	if err != nil {
		return "", fmt.Errorf("month: %w", err)
	}
	weekday, err := translateField(dow, 0, 7, weekdayName)
	if err != nil {
		return "", fmt.Errorf("day of week: %w", err)
	}

	out := fmt.Sprintf("*-%s-%s %s:%s:00", mon, day, hr, min)
	if weekday != "*" {
		out = weekday + " " + out
	}
	return out, nil
}

// weekdayNames maps cron's numbering, in which both 0 and 7 are Sunday.
var weekdayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

func weekdayName(n int) string { return weekdayNames[n] }

// translateField converts one cron field. render, when set, names a value
// instead of printing it — weekdays read as names on the host side.
func translateField(f string, lo, hi int, render func(int) string) (string, error) {
	if f == "*" {
		return "*", nil
	}
	parts := strings.Split(f, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value, step, hasStep := strings.Cut(part, "/")
		var stepN int
		if hasStep {
			n, err := strconv.Atoi(step)
			if err != nil || n < 1 {
				return "", fmt.Errorf("%q is not a step this contract accepts", part)
			}
			stepN = n
		}

		switch {
		case value == "*":
			if !hasStep {
				return "", fmt.Errorf("%q is not a value this contract accepts", part)
			}
			// `*/s` starts at the bottom of the range, which is what cron means.
			out = append(out, fmt.Sprintf("%s/%d", show(lo, render), stepN))
		case strings.Contains(value, "-"):
			fromS, toS, _ := strings.Cut(value, "-")
			from, err1 := boundedInt(fromS, lo, hi)
			to, err2 := boundedInt(toS, lo, hi)
			if err1 != nil || err2 != nil {
				return "", fmt.Errorf("%q is not a range within %d-%d", part, lo, hi)
			}
			if from > to {
				return "", fmt.Errorf("%q runs backwards", part)
			}
			r := show(from, render) + ".." + show(to, render)
			if hasStep {
				r += fmt.Sprintf("/%d", stepN)
			}
			out = append(out, r)
		default:
			n, err := boundedInt(value, lo, hi)
			if err != nil {
				return "", fmt.Errorf("%q is not a value within %d-%d", part, lo, hi)
			}
			if hasStep {
				// `n/s` means from n, every s — the same on both sides.
				out = append(out, fmt.Sprintf("%s/%d", show(n, render), stepN))
				continue
			}
			out = append(out, show(n, render))
		}
	}
	return strings.Join(out, ","), nil
}

func boundedInt(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("out of range")
	}
	return n, nil
}

func show(n int, render func(int) string) string {
	if render != nil {
		return render(n)
	}
	// Two digits, because a calendar expression reads hours and minutes that
	// way and a single digit is accepted but inconsistent in a generated file.
	return fmt.Sprintf("%02d", n)
}
