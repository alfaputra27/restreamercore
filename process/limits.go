package process

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/datarhei/core/v16/psutil"
)

type Usage struct {
	CPU struct {
		NCPU    float64 // number of logical processors
		Current float64 // percent 0-100*ncpu
		Average float64 // percent 0-100*ncpu
		Max     float64 // percent 0-100*ncpu
		Limit   float64 // percent 0-100*ncpu
	}
	Memory struct {
		Current uint64  // bytes
		Average float64 // bytes
		Max     uint64  // bytes
		Limit   uint64  // bytes
	}
}

type LimitFunc func(cpu float64, memory uint64)

type LimitMode int

const (
	LimitModeHard LimitMode = 0 // Killing the process if either CPU or memory is above the limit (for a certain time)
	LimitModeSoft LimitMode = 1 // Throttling the CPU if activated, killing the process if memory is above the limit
)

type LimiterConfig struct {
	CPU     float64       // Max. CPU usage in percent 0-100 in hard mode, 0-100*ncpu in softmode
	Memory  uint64        // Max. memory usage in bytes
	WaitFor time.Duration // Duration for one of the limits has to be above the limit until OnLimit gets triggered
	OnLimit LimitFunc     // Function to be triggered if limits are exceeded
	Mode    LimitMode     // How to limit CPU usage
}

type Limiter interface {
	// Start starts the limiter with a psutil.Process.
	Start(process psutil.Process) error

	// Stop stops the limiter. The limiter can be reused by calling Start() again
	Stop()

	// Current returns the current CPU and memory values
	// Deprecated: use Usage()
	Current() (cpu float64, memory uint64)

	// Limits returns the defined CPU and memory limits. Values <= 0 means no limit
	// Deprecated: use Usage()
	Limits() (cpu float64, memory uint64)

	// Usage returns the current state of the limiter, such as current, average, max, and
	// limit values for CPU and memory.
	Usage() Usage

	// Limit enables or disables the throttling of the CPU or killing because of to much
	// memory consumption.
	Limit(enable bool) error
}

type limiter struct {
	ncpu    float64
	proc    psutil.Process
	lock    sync.Mutex
	cancel  context.CancelFunc
	onLimit LimitFunc

	cpu           float64
	cpuCurrent    float64
	cpuMax        float64
	cpuAvg        float64
	cpuAvgCounter uint64
	cpuLast       float64
	cpuLimitSince time.Time

	memory           uint64
	memoryCurrent    uint64
	memoryMax        uint64
	memoryAvg        float64
	memoryAvgCounter uint64
	memoryLast       uint64
	memoryLimitSince time.Time

	waitFor     time.Duration
	mode        LimitMode
	enableLimit bool
	cancelLimit context.CancelFunc
}

// NewLimiter returns a new Limiter
func NewLimiter(config LimiterConfig) Limiter {
	l := &limiter{
		cpu:     config.CPU,
		memory:  config.Memory,
		waitFor: config.WaitFor,
		onLimit: config.OnLimit,
		mode:    config.Mode,
	}

	if ncpu, err := psutil.CPUCounts(true); err != nil {
		l.ncpu = 1
	} else {
		l.ncpu = ncpu
	}

	if l.mode == LimitModeSoft {
		l.cpu /= l.ncpu
	}

	if l.onLimit == nil {
		l.onLimit = func(float64, uint64) {}
	}

	return l
}

func (l *limiter) reset() {
	l.cpuCurrent = 0
	l.cpuLast = 0
	l.cpuAvg = 0
	l.cpuAvgCounter = 0
	l.cpuMax = 0

	l.memoryCurrent = 0
	l.memoryLast = 0
	l.memoryAvg = 0
	l.memoryAvgCounter = 0
	l.memoryMax = 0
}

func (l *limiter) Start(process psutil.Process) error {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.proc != nil {
		return fmt.Errorf("limiter is already running")
	}

	l.reset()

	l.proc = process

	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	go l.ticker(ctx, 500*time.Millisecond)

	return nil
}

func (l *limiter) Stop() {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.proc == nil {
		return
	}

	l.cancel()

	l.proc.Stop()
	l.proc = nil

	l.reset()
}

func (l *limiter) ticker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			l.collect(t)
		}
	}
}

func (l *limiter) collect(t time.Time) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.proc == nil {
		return
	}

	if mstat, err := l.proc.VirtualMemory(); err == nil {
		l.memoryLast, l.memoryCurrent = l.memoryCurrent, mstat

		if l.memoryCurrent > l.memoryMax {
			l.memoryMax = l.memoryCurrent
		}

		l.memoryAvgCounter++

		l.memoryAvg = ((l.memoryAvg * float64(l.memoryAvgCounter-1)) + float64(l.memoryCurrent)) / float64(l.memoryAvgCounter)
	}

	if cpustat, err := l.proc.CPUPercent(); err == nil {
		l.cpuLast, l.cpuCurrent = l.cpuCurrent, cpustat.System+cpustat.User+cpustat.Other

		if l.cpuCurrent > l.cpuMax {
			l.cpuMax = l.cpuCurrent
		}

		l.cpuAvgCounter++

		l.cpuAvg = ((l.cpuAvg * float64(l.cpuAvgCounter-1)) + l.cpuCurrent) / float64(l.cpuAvgCounter)
	}

	isLimitExceeded := false

	if l.mode == LimitModeHard {
		if l.cpu > 0 {
			if l.cpuCurrent > l.cpu {
				// Current value is higher than the limit
				if l.cpuLast <= l.cpu {
					// If the previous value is below the limit, then we reached the
					// limit as of now
					l.cpuLimitSince = time.Now()
				}

				if time.Since(l.cpuLimitSince) >= l.waitFor {
					isLimitExceeded = true
				}
			}
		}

		if l.memory > 0 {
			if l.memoryCurrent > l.memory {
				// Current value is higher than the limit
				if l.memoryLast <= l.memory {
					// If the previous value is below the limit, then we reached the
					// limit as of now
					l.memoryLimitSince = time.Now()
				}

				if time.Since(l.memoryLimitSince) >= l.waitFor {
					isLimitExceeded = true
				}
			}
		}
	} else if l.mode == LimitModeSoft && l.enableLimit {
		if l.memory > 0 {
			if l.memoryCurrent > l.memory {
				// Current value is higher than the limit
				isLimitExceeded = true
			}
		}
	}

	if isLimitExceeded {
		go l.onLimit(l.cpuCurrent, l.memoryCurrent)
	}
}

func (l *limiter) Limit(enable bool) error {
	if enable {
		if l.enableLimit {
			return nil
		}

		if l.cancelLimit != nil {
			l.cancelLimit()
		}

		ctx, cancel := context.WithCancel(context.Background())
		l.cancelLimit = cancel

		l.enableLimit = true

		go l.limit(ctx, l.cpu/100, time.Second)
	} else {
		if !l.enableLimit {
			return nil
		}

		if l.cancelLimit == nil {
			return nil
		}

		l.cancelLimit()
		l.cancelLimit = nil
	}

	return nil
}

// limit will limit the CPU usage of this process. The limit is the max. CPU usage
// normed to 0-1. The interval defines how long a time slot is that will be splitted
// into sleeping and working.
func (l *limiter) limit(ctx context.Context, limit float64, interval time.Duration) {
	var workingrate float64 = -1

	for {
		select {
		case <-ctx.Done():
			return
		default:
			l.lock.Lock()
			pcpu := l.cpuCurrent / 100
			l.lock.Unlock()

			if workingrate < 0 {
				workingrate = limit
			} else {
				workingrate = math.Min(workingrate/pcpu*limit, 1)
			}

			worktime := float64(interval.Nanoseconds()) * workingrate
			sleeptime := float64(interval.Nanoseconds()) - worktime

			l.proc.Resume()
			time.Sleep(time.Duration(worktime) * time.Nanosecond)

			if sleeptime > 0 {
				l.proc.Suspend()
				time.Sleep(time.Duration(sleeptime) * time.Nanosecond)
			}
		}
	}
}

func (l *limiter) Current() (cpu float64, memory uint64) {
	l.lock.Lock()
	defer l.lock.Unlock()

	cpu = l.cpuCurrent
	memory = l.memoryCurrent

	return
}

func (l *limiter) Usage() Usage {
	l.lock.Lock()
	defer l.lock.Unlock()

	usage := Usage{}

	usage.CPU.NCPU = l.ncpu
	usage.CPU.Limit = l.cpu * l.ncpu
	usage.CPU.Current = l.cpuCurrent * l.ncpu
	usage.CPU.Average = l.cpuAvg * l.ncpu
	usage.CPU.Max = l.cpuMax * l.ncpu

	usage.Memory.Limit = l.memory
	usage.Memory.Current = l.memoryCurrent
	usage.Memory.Average = l.memoryAvg
	usage.Memory.Max = l.memoryMax

	return usage
}

func (l *limiter) Limits() (cpu float64, memory uint64) {
	return l.cpu, l.memory
}
