package scheduler

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/sp-yduck/cluster-api-provider-proxmox/cloud/scheduler/framework"
	"github.com/sp-yduck/cluster-api-provider-proxmox/cloud/scheduler/plugins"
	"github.com/sp-yduck/proxmox-go/api"
	"github.com/sp-yduck/proxmox-go/proxmox"
)

var (
	// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule qemus.
	ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule qemus")

	// ErrNoVMIDAvailable is used to describe the error that no vmid available to schedule qemus.
	ErrNoVMIDAvailable = fmt.Errorf("no vmid available to schedule qemus")
)

// manager initiates scheduler
type Manager struct {
	params SchedulerParams
}

func NewManager(params SchedulerParams) *Manager {
	return &Manager{params: params}
}

func (m *Manager) NewScheduler(client *proxmox.Service) *Scheduler {
	logger := m.params.Logger.WithName("[qemu-scheduler]")
	nodeScheduler := NodeScheduler{filterPlugins: plugins.NewNodeFilterPlugins(), scorePlugins: plugins.NewNodeScorePlugins()}
	return &Scheduler{client: client, nodeScheduler: nodeScheduler, logger: logger}
}

type Scheduler struct {
	client        *proxmox.Service
	nodeScheduler NodeScheduler
	vmidScheduler VMIDScheduler
	logger        logr.Logger
}

type SchedulerParams struct {
	Logger logr.Logger
}

type NodeScheduler struct {
	filterPlugins []framework.NodeFilterPlugin
	scorePlugins  []framework.NodeScorePlugin
}

type VMIDScheduler struct {
}

// just poc codes
// return nextID fetched from Proxmox rest API nextID endpoint
func (s *Scheduler) GetID(ctx context.Context) (int, error) {
	return s.client.RESTClient().GetNextID(ctx)
}

func (s *Scheduler) SelectNode(ctx context.Context, config api.VirtualMachineCreateOptions) (string, error) {
	s.logger.Info("finding proxmox node matching qemu")
	nodes, err := s.client.Nodes(ctx)
	if err != nil {
		return "", err
	}

	// filter
	nodelist, _ := s.RunFilterPlugins(ctx, nil, config, nodes)
	if len(nodelist) == 0 {
		return "", ErrNoNodesAvailable
	}
	if len(nodelist) == 1 {
		return nodelist[0].Node, nil
	}

	// score
	scorelist, status := s.RunScorePlugins(ctx, nil, config, nodelist)
	if !status.IsSuccess() {
		s.logger.Error(status.Error(), "scoring failed")
	}
	selectedNode, err := selectHighestScoreNode(scorelist)
	if err != nil {
		return "", err
	}
	s.logger.Info(fmt.Sprintf("proxmox node %s was selected for vm %s", selectedNode, config.Name))
	return selectedNode, nil
}

func (s *Scheduler) RunFilterPlugins(ctx context.Context, state *framework.CycleState, config api.VirtualMachineCreateOptions, nodes []*api.Node) ([]*api.Node, error) {
	s.logger.Info("filtering proxmox node")
	feasibleNodes := make([]*api.Node, 0, len(nodes))
	nodeInfos, err := framework.GetNodeInfoList(ctx, s.client)
	if err != nil {
		return nil, err
	}
	for _, nodeInfo := range nodeInfos {
		status := framework.NewStatus()
		for _, pl := range s.nodeScheduler.filterPlugins {
			status = pl.Filter(ctx, state, config, nodeInfo)
			if !status.IsSuccess() {
				status.SetFailedPlugin(pl.Name())
				break
			}
		}
		if status.IsSuccess() {
			feasibleNodes = append(feasibleNodes, nodeInfo.Node())
		}
	}
	return feasibleNodes, nil
}

func (s *Scheduler) RunScorePlugins(ctx context.Context, state *framework.CycleState, config api.VirtualMachineCreateOptions, nodes []*api.Node) (framework.NodeScoreList, *framework.Status) {
	s.logger.Info("scoring proxmox node")
	var scoresMap map[string](map[int]framework.NodeScore)
	nodeInfos, err := framework.GetNodeInfoList(ctx, s.client)
	if err != nil {
		status := framework.NewStatus()
		status.SetCode(1)
		return nil, status
	}
	for index, nodeInfo := range nodeInfos {
		for _, pl := range s.nodeScheduler.scorePlugins {
			score, status := pl.Score(ctx, state, config, nodeInfo)
			if !status.IsSuccess() {
				return nil, status
			}
			scoresMap[pl.Name()][index] = framework.NodeScore{
				Name:  nodeInfo.Node().Node,
				Score: score,
			}
		}
	}
	result := make(framework.NodeScoreList, 0, len(nodes))
	for i := range nodes {
		result = append(result, framework.NodeScore{Name: nodes[i].Node, Score: 0})
		for j := range scoresMap {
			result[i].Score += scoresMap[j][i].Score
		}
	}
	return result, nil
}

func selectHighestScoreNode(scoreList framework.NodeScoreList) (string, error) {
	if len(scoreList) == 0 {
		return "", fmt.Errorf("empty node score list")
	}
	selectedScore := framework.NodeScore{Score: -1}
	for _, nodescore := range scoreList {
		if selectedScore.Score < nodescore.Score {
			selectedScore = nodescore
		}
	}
	return selectedScore.Name, nil
}
