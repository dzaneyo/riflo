package app

import "context"

// UnavailableService keeps the Phase 0 binary useful for doctor/serve and
// gives future adapters a stable response while media/task work is pending.
type UnavailableService struct{}

func NewUnavailableService() Service { return UnavailableService{} }

func (UnavailableService) Inspect(context.Context, InspectRequest) (MediaInfo, error) {
	return MediaInfo{}, NotImplemented("inspect")
}

func (UnavailableService) CreateTask(context.Context, CreateTaskRequest) (Task, error) {
	return Task{}, NotImplemented("download tasks")
}

func (UnavailableService) ListTasks(context.Context, TaskFilter) ([]Task, error) {
	return nil, NotImplemented("task listing")
}

func (UnavailableService) GetTask(context.Context, string) (Task, error) {
	return Task{}, NotImplemented("task lookup")
}

func (UnavailableService) CancelTask(context.Context, string) (Task, error) {
	return Task{}, NotImplemented("task cancellation")
}
