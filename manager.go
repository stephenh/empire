package empire

import (
	"fmt"

	"github.com/remind101/empire/scheduler"
)

// Job represents a Job that was submitted to the scheduler.
type Job struct {
	App         AppName
	Release     ReleaseVersion
	ProcessType ProcessType
	Instance    int

	Environment Vars
	Image       Image
	Command     Command
}

type JobState struct {
	Job       *Job
	MachineID string
	Name      scheduler.JobName
	State     string
}

func (j *Job) JobName() scheduler.JobName {
	return newJobName(
		j.App,
		j.Release,
		j.ProcessType,
		j.Instance,
	)
}

// JobQuery is a query object to filter results from JobsRepository List.
type JobQuery struct {
	App     AppName
	Release ReleaseVersion
}

// JobsRepository keeps track of all the Jobs that have been submitted to the
// scheduler.
type JobsRepository interface {
	Add(*Job) error
	Remove(*Job) error
	List(JobQuery) ([]*Job, error)
}

type jobsRepository struct {
	jobs map[scheduler.JobName]*Job
}

func newJobsRepository() *jobsRepository {
	return &jobsRepository{
		jobs: make(map[scheduler.JobName]*Job),
	}
}

func (r *jobsRepository) Add(j *Job) error {
	r.jobs[j.JobName()] = j
	return nil
}

func (r *jobsRepository) Remove(j *Job) error {
	delete(r.jobs, j.JobName())
	return nil
}

func (r *jobsRepository) List(q JobQuery) ([]*Job, error) {
	var jobs []*Job

	for _, j := range r.jobs {
		if (string(q.App) == "" || q.App == j.App) && (string(q.Release) == "" || q.Release == j.Release) {
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

// Manager is responsible for talking to the scheduler to schedule jobs onto the
// cluster.
type Manager interface {
	// ScheduleRelease schedules a release onto the cluster.
	ScheduleRelease(*Release) error

	// ScaleRelease scales a release based on a process quantity map.
	ScaleRelease(*Release, ProcessQuantityMap) error

	// FindJobsByApp returns JobStates for an app.
	JobStatesByApp(*App) ([]*JobState, error)
}

// manager is a base implementation of the Manager interface.
type manager struct {
	scheduler.Scheduler
	JobsRepository
}

// NewManager returns a new Service instance.
func NewManager(options Options) (Manager, error) {
	s, err := scheduler.NewScheduler(options.Fleet.API)
	if err != nil {
		return nil, err
	}

	return &manager{
		Scheduler:      s,
		JobsRepository: newJobsRepository(),
	}, nil
}

// ScheduleRelease creates jobs for every process and instance count and
// schedules them onto the cluster.
func (m *manager) ScheduleRelease(release *Release) error {
	// Find any existing jobs that have been scheduled for this release.
	existing, err := m.existingJobs(release)
	if err != nil {
		return err
	}

	jobs := buildJobs(
		release.App.Name,
		release.Version,
		*release.Slug.Image,
		release.Config.Vars,
		release.Formation,
	)

	if len(existing) > len(jobs) {
		remove := existing[len(jobs):]

		if err := m.unscheduleMulti(remove); err != nil {
			return err
		}
	}

	return m.scheduleMulti(jobs)
}

func (m *manager) existingJobs(release *Release) ([]*Job, error) {
	return m.JobsRepository.List(JobQuery{
		App:     release.App.Name,
		Release: release.Version,
	})
}

func (m *manager) scheduleMulti(jobs []*Job) error {
	for _, j := range jobs {
		if err := m.schedule(j); err != nil {
			return err
		}
	}

	return nil
}

// schedule schedules a Job and adds it to the list of scheduled jobs.
func (m *manager) schedule(j *Job) error {
	name := j.JobName()
	env := environment(j.Environment)
	exec := scheduler.Execute{
		Command: string(j.Command),
		Image: scheduler.Image{
			Repo: string(j.Image.Repo),
			ID:   j.Image.ID,
		},
	}

	// Schedule the job onto the cluster.
	if err := m.Scheduler.Schedule(&scheduler.Job{
		Name:        name,
		Environment: env,
		Execute:     exec,
	}); err != nil {
		return err
	}

	// Add it to the list of scheduled jobs.
	if err := m.JobsRepository.Add(j); err != nil {
		return err
	}

	return nil
}

func (m *manager) unscheduleMulti(jobs []*Job) error {
	for _, j := range jobs {
		if err := m.unschedule(j); err != nil {
			return err
		}
	}

	return nil
}

func (m *manager) unschedule(j *Job) error {
	return m.Scheduler.Unschedule(j.JobName())
}

// ScaleRelease takes a release and process quantity map, and
// schedules/unschedules jobs to make the formation match the quantity map
func (m *manager) ScaleRelease(release *Release, qm ProcessQuantityMap) error {
	f := release.Formation

	for t, q := range qm {
		if p, ok := f[t]; ok {
			if err := m.scaleProcess(release, t, p, q); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *manager) scaleProcess(release *Release, t ProcessType, p *Process, q int) error {
	// Scale up
	if p.Quantity < q {
		for i := p.Quantity + 1; i <= q; i++ {
			err := m.schedule(
				&Job{
					App:         release.App.Name,
					Release:     release.Version,
					ProcessType: t,
					Instance:    i,
					Environment: release.Config.Vars,
					Image:       *release.Slug.Image,
					Command:     p.Command,
				},
			)
			if err != nil {
				return err
			}
		}
	}

	// Scale down
	if p.Quantity > q {
		for i := p.Quantity; i >= q; i-- {
			err := m.Scheduler.Unschedule(newJobName(release.App.Name, release.Version, t, i))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *manager) JobStatesByApp(app *App) ([]*JobState, error) {
	// Jobs expected to be running
	jobs, err := m.JobsRepository.List(JobQuery{App: app.Name})
	if err != nil {
		return nil, err
	}

	// Job states for all existing jobs
	sjs, err := m.Scheduler.JobStates()
	if err != nil {
		return nil, err
	}

	// Create a map for easy lookups
	jsm := make(map[scheduler.JobName]*scheduler.JobState, len(sjs))
	for _, js := range sjs {
		jsm[js.Name] = js
	}

	// Create JobState based on Jobs and scheduler.JobStates
	js := make([]*JobState, len(jobs))
	for i, j := range jobs {
		s, ok := jsm[j.JobName()]

		machineID := "unknown"
		state := "unknown"
		if ok {
			machineID = s.MachineID
			state = s.State
		}

		js[i] = &JobState{
			Job:       j,
			Name:      j.JobName(),
			MachineID: machineID,
			State:     state,
		}
	}

	return js, nil
}

// newJobName returns a new Name with the proper format.
func newJobName(name AppName, v ReleaseVersion, t ProcessType, i int) scheduler.JobName {
	return scheduler.JobName(fmt.Sprintf("%s.%d.%s.%d", name, v, t, i))
}

func buildJobs(name AppName, version ReleaseVersion, image Image, vars Vars, f Formation) []*Job {
	var jobs []*Job

	// Build jobs for each process type
	for t, p := range f {
		// Build a Job for each instance of the process.
		for i := 1; i <= p.Quantity; i++ {
			j := &Job{
				App:         name,
				Release:     version,
				ProcessType: t,
				Instance:    i,
				Environment: vars,
				Image:       image,
				Command:     p.Command,
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