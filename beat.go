package beat

import (
	"context"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

type JobFunc func(ctx context.Context, userdata any)

type job struct {
	Id       string  // 任务ID
	Func     JobFunc // 定时执行的任务
	Userdata any     // 用户数据

	Schedule Schedule  // 定时时间
	Next     time.Time // 下一次运行的时间
	Prev     time.Time // 前一次运行的时间
}

type Beat struct {
	jobs          []*job              // 任务集合
	jobWaiter     sync.WaitGroup      // 任务完成等待
	withRecovery  bool                // 是否启用recover
	lock          sync.Mutex          // 互斥锁
	maxGoroutines int                 // 最大协程数量
	sem           *semaphore.Weighted //
	running       bool                // 是否运行
	parser        ScheduleParser      // 解析器
	location      *time.Location      // 时区
	ctx           context.Context     // 上下文
	log           Logger              // log

	operate chan any
}

type ScheduleParser interface {
	Parse(expr string) (Schedule, error)
}

type Schedule interface {
	// 根据给定时间，返回下一个可用的时间
	Next(time.Time) time.Time
}

// 排序需要用到的接口
type jobByTime []*job

func (s jobByTime) Len() int {
	return len(s)
}

func (s jobByTime) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s jobByTime) Less(i, j int) bool {
	if s[i].Next.IsZero() {
		return false
	}
	if s[j].Next.IsZero() {
		return true
	}

	return s[i].Next.Before(s[j].Next)
}

type (
	opAdd             *job
	opRemove          string
	opRemoveAll       struct{}
	opRemoveByPattern *regexp.Regexp
	opStop            struct{}
)

func emptyJobFunc(_ context.Context, _ any) {}

func New(opts ...option) *Beat {
	b := &Beat{
		jobs:     []*job{},
		parser:   defaultParser,
		location: time.Local,
		ctx:      context.Background(),
		log:      defaultLogger,

		operate: make(chan any),
	}

	for _, opt := range opts {
		opt(b)
	}

	if b.maxGoroutines > 0 {
		b.sem = semaphore.NewWeighted(int64(b.maxGoroutines))
	}

	return b
}

func (b *Beat) run() {
	b.log.Info("msg", "started")
	defer b.log.Info("msg", "stopped")

	now := b.now()

	// 获取一次所有任务的下一次有效时间
	for _, job := range b.jobs {
		job.Next = job.Schedule.Next(now)
		b.log.Info("job.action", "schedule", "job.id", job.Id, "job.next", job.Next.Format(time.RFC3339))
	}

	for {
		// 对任务的下一次执行时间进行排序，
		sort.Sort(jobByTime(b.jobs))

		var timer *time.Timer
		if len(b.jobs) == 0 || b.jobs[0].Next.IsZero() {
			// 没有任务或者时间太长，则休眠，依然可以处理添加或者停止请求
			//
			// 目前 parser 的最长时间为 2 年，防止休眠时间过长错过 2 年后
			// 的任务，此处休眠时间暂定为 1 年 (8760个小时)
			timer = time.NewTimer(8760 * time.Hour)
		} else {
			// 获取最近执行时间的定时
			timer = time.NewTimer(b.jobs[0].Next.Sub(now))
		}

		for {
			select {
			case now = <-timer.C:
				now = now.In(b.location)
				b.log.Debug("job.action", "wake")

				// 执行所有已经到定时的任务
				for _, job := range b.jobs {
					if job.Next.After(now) || job.Next.IsZero() {
						break
					}
					b.log.Debug("job.action", "execute", "job.id", job.Id)
					b.executeJob(job)

					job.Prev = job.Next
					job.Next = job.Schedule.Next(now)
				}

			case op := <-b.operate:
				timer.Stop()
				now = b.now()

				switch arg := op.(type) {
				case opAdd:
					newJob := (*job)(arg)

					newJob.Next = newJob.Schedule.Next(now)
					b.addJob(newJob)

					b.log.Info("job.action", "add", "job.id", newJob.Id, "job.next", newJob.Next.Format(time.RFC3339))

				case opRemove:
					id := string(arg)

					b.removeJob(id)

					b.log.Info("job.action", "remove", "job.id", id)

				case opRemoveAll:
					b.removeAllJob()

					b.log.Info("job.action", "remove-all")

				case opRemoveByPattern:
					pattern := (*regexp.Regexp)(arg)

					b.removeJobByPattern(pattern)

					b.log.Info("job.action", "remove-by-pattern", "job.pattern", pattern.String())

				case opStop:
					return
				}
			}

			break
		}
	}
}

// 返回 b.location 的当前时间
func (b *Beat) now() time.Time {
	return time.Now().In(b.location)
}

// 开始执行任务，任务将在协程中执行
func (b *Beat) executeJob(job *job) {
	if b.sem != nil {
		b.sem.Acquire(b.ctx, 1)
	}

	b.jobWaiter.Add(1)

	go func() {
		if b.withRecovery {
			defer func() {
				if r := recover(); r != nil {
					buf := make([]byte, 64<<10)
					n := runtime.Stack(buf, false)
					buf = buf[:n]
					b.log.Error("panic", r, "statck", string(buf))
				}
			}()
		}

		defer b.jobWaiter.Done()

		if b.sem != nil {
			defer b.sem.Release(1)
		}

		job.Func(b.ctx, job.Userdata)
	}()
}

func (b *Beat) addJob(job *job) {
	found := b.find(job.Id)
	if found != nil {
		b.log.Warn("msg", "job already exists, overwrite the old one", "job.id", found.Id)
		b.removeJob(found.Id)
	}

	b.jobs = append(b.jobs, job)
}

// 移除任务
//
// 返回移除的任务对象，不存在则返回 nil
func (b *Beat) removeJob(id string) {
	jobs := make([]*job, 0)

	for _, job := range b.jobs {
		if job.Id != id {
			jobs = append(jobs, job)
		}
	}
	b.jobs = jobs
}

// 移除全部任务
func (b *Beat) removeAllJob() {
	b.jobs = make([]*job, 0)
}

// 通过ID前缀移除任务，所有任务ID含有指定前缀的任务都将移除
func (b *Beat) removeJobByPattern(pattern *regexp.Regexp) {
	jobs := make([]*job, 0)

	for _, job := range b.jobs {
		if !pattern.MatchString(job.Id) {
			jobs = append(jobs, job)
		}
	}

	b.jobs = jobs
}

// 通过 ID 查找任务
//
// 返回查找到的任务对象，不存在则返回 nil
func (b *Beat) find(id string) *job {
	for _, job := range b.jobs {
		if job.Id == id {
			return job
		}
	}

	return nil
}

// 添加任务
//
// 参数：
//
//	expr: 定时表达式
//	id: 任务ID，每个任务ID唯一
//	fn: 任务执行回调
//	userdata: 用于保存用户数据，回调时将传递该数据
func (b *Beat) Add(expr string, id string, fn JobFunc, userdata any) error {
	sched, err := b.parser.Parse(expr)
	if err != nil {
		return err
	}

	b.lock.Lock()
	defer b.lock.Unlock()

	job := &job{
		Id:       id,
		Schedule: sched,
		Func:     fn,
		Userdata: userdata,
	}
	if job.Func == nil {
		job.Func = emptyJobFunc
	}

	if !b.running {
		b.addJob(job)
	} else {
		b.operate <- opAdd(job)
	}

	return nil
}

// 移除任务
func (b *Beat) Remove(id string) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if !b.running {
		b.removeJob(id)
	} else {
		b.operate <- opRemove(id)
	}
}

// 清空任务
func (b *Beat) RemoveAll() {
	b.lock.Lock()
	defer b.lock.Unlock()

	if !b.running {
		b.removeAllJob()
	} else {
		b.operate <- opRemoveAll(struct{}{})
	}
}

// 通过正则表达式移除任务
func (b *Beat) RemoveByPattern(exp string) error {
	b.lock.Lock()
	defer b.lock.Unlock()

	pattern, err := regexp.Compile(exp)
	if err != nil {
		return err
	}

	if !b.running {
		b.removeJobByPattern(pattern)
	} else {
		b.operate <- opRemoveByPattern(pattern)
	}

	return nil
}

// 停止运行
func (b *Beat) Stop() {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.running {
		b.operate <- opStop(struct{}{})
		b.running = false
	}
	b.jobWaiter.Wait()
}

// 开始运行，beat 将在协程中运行
func (b *Beat) Start() {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.running {
		return
	}

	b.running = true
	go b.run()
}

// 开始运行，beat 将阻塞运行
func (b *Beat) Run() {
	b.lock.Lock()

	if b.running {
		b.lock.Unlock()
		return
	}

	b.running = true
	b.lock.Unlock()
	b.run()
}

// 获取运行状态
func (b *Beat) IsRunning() bool {
	b.lock.Lock()
	defer b.lock.Unlock()

	return b.running
}

func (b *Beat) SetLogger(log Logger) {
	b.lock.Lock()
	defer b.lock.Unlock()

	b.log = log
}
