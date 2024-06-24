package cluster

import (
	"errors"

	"github.com/datarhei/core/v16/cluster/store"
)

func (c *cluster) ListNodes() map[string]store.Node {
	return c.store.ListNodes()
}

var ErrUnsupportedNodeState = errors.New("unsupported node state")

func (c *cluster) SetNodeState(origin, id, state string) error {
	switch state {
	case "online":
	case "maintenance":
	case "leave":
	default:
		return ErrUnsupportedNodeState
	}

	if !c.IsRaftLeader() {
		return c.forwarder.SetNodeState(origin, id, state)
	}

	cmd := &store.Command{
		Operation: store.OpSetNodeState,
		Data: &store.CommandSetNodeState{
			NodeID: id,
			State:  state,
		},
	}

	return c.applyCommand(cmd)
}
