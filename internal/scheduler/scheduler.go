// Package scheduler 实现优先级队列、同优先级 FIFO 和串行状态转换。
package scheduler

import (
	"capybara/internal/gpu"
	"capybara/internal/model"
	"capybara/internal/runner"
	"capybara/internal/store"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Scheduler 的锁覆盖提交、取消和调度，防止启动与取消交错。
type Scheduler struct {
	mu       sync.Mutex
	Store    *store.Store
	Runner   runner.Runner
	Probe    func(context.Context) ([]gpu.Device, error)
	Limit    time.Duration
	active   map[int64]context.CancelFunc
	wake     chan struct{}
	stopping bool
	wg       sync.WaitGroup
}

// New 创建调度器。
func New(s *store.Store, r runner.Runner, p func(context.Context) ([]gpu.Device, error), limit time.Duration) *Scheduler {
	return &Scheduler{Store: s, Runner: r, Probe: p, Limit: limit, active: map[int64]context.CancelFunc{}, wake: make(chan struct{}, 1)}
}

// Wake 合并重复通知，定时器负责兜底。
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Submit 将作业持久化后唤醒队列。
func (s *Scheduler) Submit(j *model.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return fmt.Errorf("服务正在停止")
	}
	if !model.ValidPriority(j.Priority) {
		return fmt.Errorf("优先级必须在 -100 到 100 之间")
	}
	if e := s.Store.Create(j); e != nil {
		return e
	}
	s.Wake()
	return nil
}

// Cancel 对排队作业立即落库，运行作业由执行器确认退出后落库。
func (s *Scheduler) Cancel(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, e := s.Store.Get(id)
	if e != nil {
		return e
	}
	if model.Terminal(j.State) {
		return nil
	}
	if c := s.active[id]; c != nil {
		c()
		return nil
	}
	if j.State == "running" {
		return fmt.Errorf("作业需要人工确认，无法安全取消")
	}
	now := time.Now().UTC()
	j.EndAt = &now
	j.State = "cancelled"
	return s.Store.Transition(j, "pending")
}

// SetPriority 在与调度相同的锁内更新排队作业，防止启动后被旧快照覆盖。
func (s *Scheduler) SetPriority(id int64, priority int) (model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !model.ValidPriority(priority) {
		return model.Job{}, fmt.Errorf("优先级必须在 -100 到 100 之间")
	}
	j, err := s.Store.Get(id)
	if err != nil {
		return j, err
	}
	if j.State != "pending" {
		return j, fmt.Errorf("只能修改 pending 作业的优先级")
	}
	j.Priority = priority
	if err = s.Store.Transition(j, "pending"); err != nil {
		return j, err
	}
	s.Wake()
	return j, nil
}

// Nodes 合并设备采样和持久化分配；监控与 CLI 使用相同判断。
func (s *Scheduler) Nodes(ctx context.Context) ([]gpu.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	devices, err := s.Probe(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.Store.List()
	if err != nil {
		return nil, err
	}
	for i := range devices {
		for _, j := range jobs {
			if j.State != "running" {
				continue
			}
			for _, uuid := range j.GPUs {
				if uuid == devices[i].UUID {
					devices[i].JobID = j.ID
					devices[i].External = false
				}
			}
		}
	}
	return devices, nil
}

// Recover 保留旧运行作业的分配，避免无法确认进程状态时重新使用 GPU。
func (s *Scheduler) Recover() error {
	jobs, e := s.Store.List()
	if e != nil {
		return e
	}
	for _, j := range jobs {
		if j.State == "running" {
			slog.Warn("发现未接管的运行作业，保留 GPU 分配，需人工确认", "job", j.ID)
		}
	}
	return nil
}

// Run 收到停止信号后取消活跃作业并等待状态写回。shim 接管留待 M2。
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Wake()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.stopping = true
			for _, c := range s.active {
				c()
			}
			s.mu.Unlock()
			s.wg.Wait()
			return
		case <-s.wake:
			s.tick(ctx)
		case <-t.C:
			s.tick(ctx)
		}
	}
}
func (s *Scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, e := s.Store.List()
	if e != nil {
		slog.Error("读取队列失败", "error", e)
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	devices, probeErr := s.Probe(probeCtx)
	cancel()
	if probeErr != nil {
		slog.Warn("GPU 探测失败，暂停 GPU 分配", "error", probeErr)
	}
	used := map[string]bool{}
	for _, j := range jobs {
		if j.State == "running" {
			for _, id := range j.GPUs {
				used[id] = true
			}
		}
	}
	free := []string{}
	for _, d := range devices {
		if !used[d.UUID] && !d.External {
			free = append(free, d.UUID)
		}
	}
	model.SortQueue(jobs)
	for _, j := range jobs {
		if j.State != "pending" {
			continue
		}
		if j.NGPUs > 0 && (probeErr != nil || len(free) < j.NGPUs) {
			break
		}
		j.GPUs = append([]string{}, free[:j.NGPUs]...)
		now := time.Now().UTC()
		j.StartAt = &now
		j.State = "running"
		if e = s.Store.Transition(j, "pending"); e != nil {
			slog.Error("分配失败", "error", e)
			return
		}
		free = free[j.NGPUs:]
		limit := j.TimeLimit
		if limit <= 0 {
			limit = s.Limit
		}
		runCtx, c := context.WithTimeout(context.Background(), limit)
		s.active[j.ID] = c
		s.wg.Add(1)
		go s.execute(runCtx, c, j)
	}
}
func (s *Scheduler) execute(ctx context.Context, cancel context.CancelFunc, j model.Job) {
	defer s.wg.Done()
	defer cancel()
	code, err := s.Runner.Run(ctx, j)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, j.ID)
	// 无法确认进程组死亡时，保持 running 和分配记录，禁止重新分配。
	if errors.Is(err, runner.ErrUnconfirmed) {
		slog.Error("作业终止未确认，保留分配", "job", j.ID, "error", err)
		return
	}
	now := time.Now().UTC()
	j.EndAt = &now
	j.ExitCode = &code
	j.State = "succeeded"
	if err != nil {
		j.State = "failed"
		j.Reason = err.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		j.State = "timeout"
	} else if ctx.Err() != nil {
		j.State = "cancelled"
	}
	if e := s.Store.Transition(j, "running"); e != nil {
		slog.Error("写入作业结果失败", "job", j.ID, "error", e)
	}
	s.Wake()
}
