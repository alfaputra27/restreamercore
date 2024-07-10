package api

import (
	"github.com/datarhei/core/v16/restream/app"
)

type AVstreamIO struct {
	State  string `json:"state" enums:"running,idle" jsonschema:"enum=running,enum=idle"`
	Packet uint64 `json:"packet" format:"uint64"`
	Time   uint64 `json:"time"`
	Size   uint64 `json:"size_kb"`
}

func (i *AVstreamIO) Unmarshal(io *app.AVstreamIO) {
	if io == nil {
		return
	}

	i.State = io.State
	i.Packet = io.Packet
	i.Time = io.Time
	i.Size = io.Size
}

func (i *AVstreamIO) Marshal() app.AVstreamIO {
	io := app.AVstreamIO{
		State:  i.State,
		Packet: i.Packet,
		Time:   i.Time,
		Size:   i.Size,
	}

	return io
}

type AVstream struct {
	Input          AVstreamIO `json:"input"`
	Output         AVstreamIO `json:"output"`
	Aqueue         uint64     `json:"aqueue" format:"uint64"`
	Queue          uint64     `json:"queue" format:"uint64"`
	Dup            uint64     `json:"dup" format:"uint64"`
	Drop           uint64     `json:"drop" format:"uint64"`
	Enc            uint64     `json:"enc" format:"uint64"`
	Looping        bool       `json:"looping"`
	LoopingRuntime uint64     `json:"looping_runtime" format:"uint64"`
	Duplicating    bool       `json:"duplicating"`
	GOP            string     `json:"gop"`
	Mode           string     `json:"mode"`
}

func (a *AVstream) Unmarshal(av *app.AVstream) {
	if av == nil {
		return
	}

	a.Aqueue = av.Aqueue
	a.Queue = av.Queue
	a.Dup = av.Dup
	a.Drop = av.Drop
	a.Enc = av.Enc
	a.Looping = av.Looping
	a.LoopingRuntime = av.LoopingRuntime
	a.Duplicating = av.Duplicating
	a.GOP = av.GOP
	a.Mode = av.Mode

	a.Input.Unmarshal(&av.Input)
	a.Output.Unmarshal(&av.Output)
}

func (a *AVstream) Marshal() *app.AVstream {
	av := &app.AVstream{
		Input:          a.Input.Marshal(),
		Output:         a.Output.Marshal(),
		Aqueue:         a.Aqueue,
		Queue:          a.Queue,
		Dup:            a.Dup,
		Drop:           a.Drop,
		Enc:            a.Enc,
		Looping:        a.Looping,
		LoopingRuntime: a.LoopingRuntime,
		Duplicating:    a.Duplicating,
		GOP:            a.GOP,
		Mode:           a.Mode,
	}

	return av
}
