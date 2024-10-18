package workers

import (
	"context"
	"math"
	"time"

	"github.com/rs/zerolog/log"
)

const secsInDay = 24 * 60 * 60

type ExecFn func(ctx context.Context, estimateTimeSecs int64) func() error

type JobMode interface {
	start(ctx context.Context, fnCh chan func() error)
}

type OnceMode struct {
	WaitTimeSecs time.Duration
	Fn           ExecFn
}

func (m *OnceMode) start(ctx context.Context, fnChan chan func() error) {
	go func() {
		select {
		case <-ctx.Done():
			close(fnChan)
		case <-time.After(m.WaitTimeSecs):
			fnChan <- m.Fn(ctx, 0)
		}
		log.Debug().Msg("once job finished")
	}()

}

type RepeatMode struct {
	RepeatInterval time.Duration
	Fn             ExecFn
}

func (m *RepeatMode) start(ctx context.Context, fnChan chan func() error) {
	t := time.NewTicker(m.RepeatInterval)

	go func() {
		for {
			select {
			case <-ctx.Done():
				close(fnChan)
				t.Stop()
				log.Debug().Msg("repeat job finished")
				return
			case <-t.C:
				fnChan <- m.Fn(ctx, int64(m.RepeatInterval.Seconds()))
			}
		}
	}()
}

type CyclicMode struct {
	Location    *time.Location
	DateBegin   time.Time
	OnFunc      ExecFn
	OffFunc     ExecFn
	OnDuration  time.Duration // in seconds
	OffDuration time.Duration // in seconds
}

func (m *CyclicMode) start(ctx context.Context, fnCh chan func() error) {
	var onTimer <-chan time.Time
	var offTimer <-chan time.Time

	log.Debug().Msgf("starting cycling timer | begin date: %v", m.DateBegin)

	// calculate time offset
	offsetDur := time.Now().
		In(m.Location).
		Sub(m.DateBegin)

	log.Debug().Msgf("time now: %v", time.Now().In(m.Location))
	log.Debug().Msgf("offset duration: %v", offsetDur)

	cycleSumDur := m.OnDuration + m.OffDuration
	nanosecsElapsedFromStart := math.Mod(float64(offsetDur.Nanoseconds()), float64(cycleSumDur.Nanoseconds()))
	durElapsedFromStart := time.Duration(int64(nanosecsElapsedFromStart))

	log.Debug().Msgf("elapsed from day start: %v", durElapsedFromStart)

	// calculate offset time and turn appropriate mode
	if durElapsedFromStart.Seconds() < m.OnDuration.Seconds() {
		dur := time.Duration(m.OnDuration.Nanoseconds() - durElapsedFromStart.Nanoseconds())
		log.Debug().Msgf("working in on mode: %v", dur)
		onTimer = time.After(time.Duration(dur))
		defer func() {
			fnCh <- m.OnFunc(ctx, int64(dur.Seconds()))
		}()
	} else {
		dur := time.Duration(cycleSumDur.Nanoseconds() - durElapsedFromStart.Nanoseconds())
		log.Debug().Msgf("working in off mode: %v", dur)
		offTimer = time.After(dur)
		defer func() {
			fnCh <- m.OffFunc(ctx, int64(dur.Seconds()))
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Debug().Msg("cyclic timer done")
				close(fnCh)
				return
			case <-onTimer:
				log.Debug().Msg("cyclic timer: sending Off function")
				dur := m.OffDuration
				fnCh <- m.OffFunc(ctx, int64(dur.Seconds()))
				offTimer = time.After(dur)
			case <-offTimer:
				log.Debug().Msg("cyclic timer: sending On function")
				dur := m.OnDuration
				fnCh <- m.OnFunc(ctx, int64(dur.Seconds()))
				onTimer = time.After(dur)
			}
		}
	}()
}

type Schedule struct {
	Table  []ScheduleTableItem `json:"table"`
	Dbegin int64               `json:"dbegin"` // start date
	Dskip  int                 `json:"dskip"`  // days to skip
}

type ScheduleTableItem struct {
	TimeOffset int `json:"t1"` // offset from start of the day in seconds
	OnTime     int `json:"t2"` // on duration in seconds
}

type ScheduleMode struct {
	Location *time.Location
	OnFunc   ExecFn
	OffFunc  ExecFn
	Schedule Schedule
}

func daySeconds(t time.Time) int {
	year, month, day := t.Date()
	t2 := time.Date(year, month, day, 0, 0, 0, 0, t.Location())
	return int(t.Sub(t2).Seconds())
}

func (m *ScheduleMode) start(ctx context.Context, fnCh chan func() error) {
	beginDate := time.Unix(m.Schedule.Dbegin, 0).In(m.Location)
	log.Debug().Msgf("starting scheduler | begin date: %v", beginDate)

	// calculate time offset
	offsetDur := time.Now().
		In(m.Location).
		Sub(beginDate)

	totalCycleSecsLen := float64((m.Schedule.Dskip + 1) * secsInDay)
	// calculate secs to skip; if mod = 0 => start today
	secsElapsedFromLastCycle := math.Mod(offsetDur.Seconds(), totalCycleSecsLen)

	log.Debug().Msgf("total cycle secs lenght: %f", totalCycleSecsLen)
	log.Debug().Msgf("secs elaplsed from start date: %d", offsetDur.Seconds())
	log.Debug().Msgf("secs elapsed from last cycle: %f", secsElapsedFromLastCycle)

	// time to wait before cycle begins in seconds
	var secsOffset int
	secsOffsetFromDbegin := daySeconds(beginDate)
	secsElapsedToday := daySeconds(time.Now().In(m.Location)) - secsOffsetFromDbegin

	if m.Schedule.Dskip == 0 {
		secsOffset = 0
	} else if offsetDur.Seconds() >= secsInDay {
		if (secsElapsedFromLastCycle / secsInDay) >= 1 {
			secsOffset = int(totalCycleSecsLen - secsElapsedFromLastCycle)
		}
	}

	// storing table index offset
	var idx int = 0
	// taking on state shift into account if schedule will be started in "on" state
	var on bool = false
	var haveJobForToday bool = false

	// calculate today offset according to schedule and secs elapsed for now
	if secsOffset == 0 {
		// offset accumulator
		var secsSum int = 0

		for i, v := range m.Schedule.Table {
			// update secs elapsed value
			secsElapsedToday = daySeconds(time.Now().In(m.Location)) - secsOffsetFromDbegin

			secsSum = v.TimeOffset
			idx = i
			// check if we waiting for "on" state
			if secsSum >= secsElapsedToday {
				secsOffset = secsSum - secsElapsedToday
				on = false
				haveJobForToday = true
				break
			}
			// check if we at "on" state now
			secsSum += v.OnTime
			if secsSum >= secsElapsedToday {
				secsOffset = secsSum - secsElapsedToday
				on = true
				haveJobForToday = true
				break
			}
		}
	}

	if !haveJobForToday {
		secsOffset = secsInDay - daySeconds(time.Now().In(m.Location)) +
			m.Schedule.Dskip*secsInDay
	}

	// execute appropriate function immediately
	if on {
		fnCh <- m.OnFunc(ctx, int64(secsOffset))
	} else {
		fnCh <- m.OffFunc(ctx, int64(secsOffset))
	}

	log.Debug().Msgf("secsOffsetFromDbegin: %d", secsOffsetFromDbegin)
	log.Debug().Msgf("secsElapsedToday: %d", secsElapsedToday)
	log.Debug().Msgf("haveJobForToday: %v", haveJobForToday)
	log.Debug().Msgf("state: %v", on)
	log.Debug().Msgf("idx: %d", idx)

	// wait for offset time
	log.Debug().Msgf("waiting cycle for start: %d", secsOffset)

	select {
	case <-time.After(time.Duration(float64(secsOffset) * float64(time.Second))):
	case <-ctx.Done():
		close(fnCh)
		return
	}

	startOfCycleTime := time.Now().In(m.Location).
		Add(-time.Duration(float64(m.Schedule.Table[idx].TimeOffset) * float64(time.Second)))

	log.Debug().Msgf("cycle start time: %v", startOfCycleTime)

	var secsWorkedTotal int = m.Schedule.Table[idx].TimeOffset
	onTimeDelta := 0
	if on {
		secsWorkedTotal += m.Schedule.Table[idx].OnTime
		onTimeDelta = m.Schedule.Table[idx].OnTime
	}

ScheduleLoop:
	scheduleItemsLen := len(m.Schedule.Table)
	for i, row := range m.Schedule.Table[idx:] {

		log.Debug().Msgf("cycle idx: %d", idx+i)

		timeToWait := row.TimeOffset - secsWorkedTotal
		timeEstimate := row.OnTime - onTimeDelta

		if timeToWait > 0 {
			log.Debug().Msgf("waiting in off state for %d secs...", timeToWait)
			select {
			case <-time.After(time.Duration(float64(timeToWait) * float64(time.Second))):
			case <-ctx.Done():
				close(fnCh)
				return
			}

			secsWorkedTotal += timeToWait
			fnCh <- m.OnFunc(ctx, int64(timeEstimate))
		}

		if timeEstimate > 0 {
			log.Debug().Msgf("turning on for %d secs...", row.OnTime)
			select {
			case <-time.After(time.Duration(float64(row.OnTime) * float64(time.Second))):
			case <-ctx.Done():
				close(fnCh)
				return
			}

			onTimeDelta = 0
			secsWorkedTotal += row.OnTime

			estTime := 0
			if i+1 < scheduleItemsLen {
				estTime = m.Schedule.Table[i+1].OnTime
			}
			// timeToWait := row.TimeOffset - secsWorkedTotal
			fnCh <- m.OffFunc(ctx, int64(estTime))
		}
	}

	log.Debug().Msg("schedule complete for today!")

	nextCycleTime := startOfCycleTime.Add(time.Duration(float64((m.Schedule.Dskip+1)*secsInDay) * float64(time.Second)))
	timeToWait := nextCycleTime.Sub(time.Now().In(m.Location))

	idx = 0
	secsWorkedTotal = 0

	log.Debug().Msgf("waiting %d for the next cycle", timeToWait.Seconds())

	select {
	case <-time.After(timeToWait):
	case <-ctx.Done():
		close(fnCh)
		return
	}

	goto ScheduleLoop

}
