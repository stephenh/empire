package empire

import (
	"fmt"

	"github.com/remind101/empire/empire/pkg/logger"
	"golang.org/x/net/context"
)

type manager struct {
	*jobsService
	store *store
}

// ScheduleRelease creates jobs for every process and instance count and
// schedules them onto the cluster.
func (m *manager) ScheduleRelease(ctx context.Context, release *Release, config *Config, slug *Slug, formation Formation) error {
	// Find any existing jobs that have been scheduled for this app.
	existing, err := m.store.JobsList(JobsListQuery{App: release.AppName})
	if err != nil {
		return err
	}

	jobs := buildJobs(
		release.AppName,
		release.Ver,
		slug.Image,
		config.Vars,
		formation,
	)

	logger.Log(ctx,
		"at", "release.schedule",
		"app", release.AppName,
		"version", release.Ver,
		"image", slug.Image,
	)

	if err := m.jobsService.Schedule(ctx, jobs...); err != nil {
		return err
	}

	if err := m.jobsService.Unschedule(ctx, existing...); err != nil {
		return err
	}

	return nil
}

func (m *manager) ScaleProcess(ctx context.Context, release *Release, config *Config, slug *Slug, p *Process, q int) error {
	var scale func(context.Context, *Release, *Config, *Slug, *Process, int) error

	switch {
	case p.Quantity < q:
		scale = m.scaleUp
	case p.Quantity > q:
		scale = m.scaleDown
	default:
		return nil
	}

	if err := scale(ctx, release, config, slug, p, q); err != nil {
		return err
	}

	// Update quantity for this process in the formation
	p.Quantity = q
	_, err := m.store.ProcessesUpdate(p)
	return err
}

func (m *manager) scaleUp(ctx context.Context, release *Release, config *Config, slug *Slug, p *Process, q int) error {
	jobs := scaleUp(release, config, slug, p, q)

	logger.Log(ctx,
		"at", "scale.up",
		"app", release.AppName,
		"process", p.Type,
		"old", p.Quantity,
		"new", q,
		"diff", fmt.Sprintf("+%d", len(jobs)),
	)

	return m.jobsService.Schedule(ctx, jobs...)
}

func (m *manager) scaleDown(ctx context.Context, release *Release, config *Config, slug *Slug, p *Process, q int) error {
	// Find existing jobs for this app
	existing, err := m.store.JobsList(JobsListQuery{
		App: release.AppName,
	})
	if err != nil {
		return err
	}

	jobs := scaleDown(existing, release, config, slug, p, q)

	logger.Log(ctx,
		"at", "scale.down",
		"app", release.AppName,
		"process", p.Type,
		"old", p.Quantity,
		"new", q,
		"diff", fmt.Sprintf("-%d", len(jobs)),
	)

	return m.jobsService.Unschedule(ctx, jobs...)
}

// scaleUp returns new Jobs to schedule when scaling up.
func scaleUp(release *Release, config *Config, slug *Slug, p *Process, q int) []*Job {
	var jobs []*Job

	for i := p.Quantity + 1; i <= q; i++ {
		jobs = append(jobs, &Job{
			AppName:        release.AppName,
			ReleaseVersion: release.Ver,
			ProcessType:    p.Type,
			Instance:       i,
			Environment:    config.Vars,
			Image:          slug.Image,
			Command:        p.Command,
		})
	}

	return jobs
}

// scaleDown returns Jobs to unschedule when scaling down.
func scaleDown(existing []*Job, release *Release, config *Config, slug *Slug, p *Process, q int) []*Job {
	// Create a map for easy lookup
	jm := make(map[string]*Job, len(existing))
	for _, j := range existing {
		jm[j.ContainerName()] = j
	}

	var jobs []*Job

	// Unschedule jobs
	for i := p.Quantity; i > q; i-- {
		jobName := newContainerName(release.AppName, release.Ver, p.Type, i)
		if j, ok := jm[jobName]; ok {
			jobs = append(jobs, j)
		}
	}

	return jobs
}

// newContainerName returns a new Name with the proper format.
func newContainerName(name string, v int, t ProcessType, i int) string {
	return fmt.Sprintf("%s.%d.%s.%d", name, v, t, i)
}

func buildJobs(name string, version int, image Image, vars Vars, f Formation) []*Job {
	var jobs []*Job

	// Build jobs for each process type
	for t, p := range f {
		// Build a Job for each instance of the process.
		for i := 1; i <= p.Quantity; i++ {
			j := &Job{
				AppName:        name,
				ReleaseVersion: version,
				ProcessType:    t,
				Instance:       i,
				Environment:    vars,
				Image:          image,
				Command:        p.Command,
			}

			jobs = append(jobs, j)
		}
	}

	return jobs
}

// environment coerces a Vars into a map[string]string.
func environment(vars Vars) map[string]string {
	env := make(map[string]string)

	for k, v := range vars {
		env[string(k)] = string(v)
	}

	return env
}
