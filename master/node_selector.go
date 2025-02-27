// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/log"
)

const RoundRobinNodeSelectorName = "RoundRobin"

const CarryWeightNodeSelectorName = "CarryWeight"

const AvailableSpaceFirstNodeSelectorName = "AvailableSpaceFirst"

const StrawNodeSelectorName = "Straw"

const DefaultNodeSelectorName = CarryWeightNodeSelectorName

var (
	ErrUnknownNodeResourceType = errors.New("unknown node resource type")
)

func (ns *nodeSet) getNodes(nodeType NodeResourceType) *sync.Map {
	switch nodeType {
	case DataNodeDisk:
		return ns.dataNodes
	case MetaNodeMemory, MetaNodeRocksdb:
		return ns.metaNodes
	default:
		panic(ErrUnknownNodeResourceType)
	}
}

type NodeSelector interface {
	GetName() string
	Select(ns *nodeSet, excludeHosts []string, replicaNum int) (newHosts []string, peers []proto.Peer, err error)
}

type weightedNode struct {
	Carry  float64
	Weight float64
	Ptr    Node
	ID     uint64
}

// Node defines an interface that needs to be implemented by weightedNode
type Node interface {
	SelectNodeForWrite(resource NodeResourceType)
	GetID() uint64
	GetAddr() string
}

// SortedWeightedNodes defines an array sorted by carry
type SortedWeightedNodes []*weightedNode

func (nodes SortedWeightedNodes) Len() int {
	return len(nodes)
}

func (nodes SortedWeightedNodes) Less(i, j int) bool {
	return nodes[i].Carry > nodes[j].Carry
}

func (nodes SortedWeightedNodes) Swap(i, j int) {
	nodes[i], nodes[j] = nodes[j], nodes[i]
}

func canAllocPartition(node interface{}, nodeType NodeResourceType) bool {
	switch nodeType {
	case DataNodeDisk:
		dataNode := node.(*DataNode)
		return dataNode.canAlloc() && dataNode.canAllocDp()
	case MetaNodeMemory:
		metaNode := node.(*MetaNode)
		return metaNode.isWritable(proto.StoreModeMem)
	case MetaNodeRocksdb:
		metaNode := node.(*MetaNode)
		return metaNode.isWritable(proto.StoreModeRocksDb)
	default:
		panic(ErrUnknownNodeResourceType)
	}
}

func asNodeWrap(node interface{}, nodeType NodeResourceType) Node {
	switch nodeType {
	case DataNodeDisk:
		dataNode := node.(*DataNode)
		return dataNode
	case MetaNodeMemory, MetaNodeRocksdb:
		metaNode := node.(*MetaNode)
		return metaNode
	default:
		panic(ErrUnknownNodeResourceType)
	}
}

type CarryWeightNodeSelector struct {
	nodeType NodeResourceType

	carry map[uint64]float64
}

func (s *CarryWeightNodeSelector) GetName() string {
	return CarryWeightNodeSelectorName
}

func (s *CarryWeightNodeSelector) prepareCarryForDataNodes(nodes *sync.Map, total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		if _, ok := s.carry[dataNode.ID]; !ok {
			// use available space to calculate initial weight
			s.carry[dataNode.ID] = float64(dataNode.AvailableSpace) / float64(total)
		}
		return true
	})
}

func (s *CarryWeightNodeSelector) prepareCarryForMetaNodeMemory(nodes *sync.Map, total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		if _, ok := s.carry[metaNode.ID]; !ok {
			// use available space to calculate initial weight
			s.carry[metaNode.ID] = float64(metaNode.Total-metaNode.Used) / float64(total)
		}
		return true
	})
}

func (s *CarryWeightNodeSelector) prepareCarryForMetaNodeRocksdb(nodes *sync.Map, total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		if _, ok := s.carry[metaNode.ID]; !ok {
			// use available space to calculate initial weight
			s.carry[metaNode.ID] = float64(metaNode.GetRocksdbTotal()-metaNode.GetRocksdbUsed()) / float64(total)
		}
		return true
	})
}

func (s *CarryWeightNodeSelector) prepareCarryForMetaNodes(nodes *sync.Map, total uint64) {
	switch s.nodeType {
	case MetaNodeMemory:
		s.prepareCarryForMetaNodeMemory(nodes, total)
	case MetaNodeRocksdb:
		s.prepareCarryForMetaNodeRocksdb(nodes, total)
	default:
	}
}

func (s *CarryWeightNodeSelector) prepareCarry(nodes *sync.Map, total uint64) {
	switch s.nodeType {
	case DataNodeDisk:
		s.prepareCarryForDataNodes(nodes, total)
	case MetaNodeMemory, MetaNodeRocksdb:
		s.prepareCarryForMetaNodes(nodes, total)
	default:
	}
}

func (s *CarryWeightNodeSelector) getTotalMaxForDataNodes(nodes *sync.Map) (total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		if dataNode.Total > total {
			total = dataNode.Total
		}
		return true
	})
	return
}

func (s *CarryWeightNodeSelector) getTotalMaxForMetaNodeMemory(nodes *sync.Map) (total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		if metaNode.Total > total {
			total = metaNode.Total
		}
		return true
	})
	return
}

func (s *CarryWeightNodeSelector) getTotalMaxForMetaNodeRocksdb(nodes *sync.Map) (total uint64) {
	nodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		nodeTotal := metaNode.GetRocksdbTotal()
		if nodeTotal > total {
			total = nodeTotal
		}
		return true
	})
	return
}

func (s *CarryWeightNodeSelector) getTotalMaxForMetaNodes(nodes *sync.Map) (total uint64) {
	switch s.nodeType {
	case MetaNodeMemory:
		return s.getTotalMaxForMetaNodeMemory(nodes)
	case MetaNodeRocksdb:
		return s.getTotalMaxForMetaNodeRocksdb(nodes)
	}
	return
}

func (s *CarryWeightNodeSelector) getTotalMax(nodes *sync.Map) (total uint64) {
	switch s.nodeType {
	case DataNodeDisk:
		total = s.getTotalMaxForDataNodes(nodes)
	case MetaNodeMemory, MetaNodeRocksdb:
		total = s.getTotalMaxForMetaNodes(nodes)
	default:
	}
	return
}

func (s *CarryWeightNodeSelector) getCarryDataNodes(maxTotal uint64, excludeHosts []string, dataNodes *sync.Map) (nodeTabs SortedWeightedNodes, availCount int) {
	nodeTabs = make(SortedWeightedNodes, 0)
	dataNodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		if contains(excludeHosts, dataNode.Addr) {
			log.LogDebugf("[getCarryDataNodes] dataNode [%v] is excludeHosts", dataNode.Addr)
			return true
		}
		if !dataNode.canAllocDp() {
			log.LogDebugf("[getCarryDataNodes] dataNode [%v] is not writeable, offline %v, dpCnt %d",
				dataNode.Addr, dataNode.ToBeOffline, dataNode.DataPartitionCount)
			return true
		}

		if !dataNode.canAlloc() {
			log.LogWarnf("[getCarryDataNodes] dataNode [%v] is overSold", dataNode.Addr)
			return true
		}
		if s.carry[dataNode.ID] >= 1.0 {
			availCount++
		}

		nt := new(weightedNode)
		nt.Carry = s.carry[dataNode.ID]
		nt.Weight = float64(dataNode.AvailableSpace) / float64(maxTotal)
		nt.Ptr = dataNode
		nodeTabs = append(nodeTabs, nt)
		return true
	})
	return
}

func (s *CarryWeightNodeSelector) getCarryMetaNodes(maxTotal uint64, excludeHosts []string, metaNodes *sync.Map) (nodes SortedWeightedNodes, availCount int) {
	nodes = make(SortedWeightedNodes, 0)
	storeMode := proto.StoreModeMem
	if s.nodeType == MetaNodeRocksdb {
		storeMode = proto.StoreModeRocksDb
	}
	metaNodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		if contains(excludeHosts, metaNode.Addr) {
			return true
		}
		if !metaNode.isWritable(storeMode) {
			return true
		}
		if s.carry[metaNode.ID] >= 1.0 {
			availCount++
		}
		nt := new(weightedNode)
		nt.Carry = s.carry[metaNode.ID]
		nt.Weight = (float64)(metaNode.Total-metaNode.Used) / (float64)(maxTotal)
		nt.Ptr = metaNode
		nodes = append(nodes, nt)
		return true
	})
	return
}

func (s *CarryWeightNodeSelector) getCarryNodes(nset *nodeSet, maxTotal uint64, excludeHosts []string) (SortedWeightedNodes, int) {
	switch s.nodeType {
	case DataNodeDisk:
		return s.getCarryDataNodes(maxTotal, excludeHosts, nset.dataNodes)
	case MetaNodeMemory, MetaNodeRocksdb:
		return s.getCarryMetaNodes(maxTotal, excludeHosts, nset.metaNodes)
	default:
		panic(ErrUnknownNodeResourceType)
	}
}

func (s *CarryWeightNodeSelector) setNodeCarry(nodes SortedWeightedNodes, availCarryCount, replicaNum int) {
	for availCarryCount < replicaNum {
		availCarryCount = 0
		for _, nt := range nodes {
			carry := nt.Carry + nt.Weight
			// limit the max value of weight
			// prevent subsequent selections make node overloading
			if carry > 10.0 {
				carry = 10.0
			}
			nt.Carry = carry
			s.carry[nt.Ptr.GetID()] = carry
			if carry > 1.0 {
				availCarryCount++
			}
		}
	}
}

func (s *CarryWeightNodeSelector) selectNodeForWrite(node Node) {
	node.SelectNodeForWrite(s.nodeType)
	// decrease node weight
	s.carry[node.GetID()] -= 1.0
}

func (s *CarryWeightNodeSelector) Select(ns *nodeSet, excludeHosts []string, replicaNum int) (newHosts []string, peers []proto.Peer, err error) {
	nodes := ns.getNodes(s.nodeType)
	total := s.getTotalMax(nodes)
	// prepare carry for every nodes
	s.prepareCarry(nodes, total)
	orderHosts := make([]string, 0)
	newHosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	// if replica == 0, return
	if replicaNum == 0 {
		return
	}
	// if we cannot get enough writable nodes, return error
	weightedNodes, count := s.getCarryNodes(ns, total, excludeHosts)
	if len(weightedNodes) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough writable hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(weightedNodes))
		return
	}
	// create enough carry nodes
	// we say a node is "carry node", whent its carry >= 1.0
	s.setNodeCarry(weightedNodes, count, replicaNum)
	// sort nodes by weight
	sort.Sort(weightedNodes)
	// pick first N nodes
	for i := 0; i < replicaNum; i++ {
		node := weightedNodes[i].Ptr
		s.selectNodeForWrite(node)
		orderHosts = append(orderHosts, node.GetAddr())
		peer := proto.Peer{ID: node.GetID(), Addr: node.GetAddr()}
		peers = append(peers, peer)
	}
	log.LogInfof("action[%vNodeSelector::Select] peers[%v]", s.GetName(), peers)
	// reshuffle for primary-backup replication
	if newHosts, err = reshuffleHosts(orderHosts); err != nil {
		err = fmt.Errorf("action[%vNodeSelector::Select] err:%v  orderHosts is nil", s.GetName(), err.Error())
		return
	}
	return
}

func NewCarryWeightNodeSelector(nodeType NodeResourceType) *CarryWeightNodeSelector {
	return &CarryWeightNodeSelector{
		carry:    make(map[uint64]float64),
		nodeType: nodeType,
	}
}

type AvailableSpaceFirstNodeSelector struct {
	nodeType NodeResourceType
}

func (s *AvailableSpaceFirstNodeSelector) getNodeAvailableSpace(node interface{}) uint64 {
	switch s.nodeType {
	case DataNodeDisk:
		dataNode := node.(*DataNode)
		return dataNode.AvailableSpace
	case MetaNodeMemory:
		metaNode := node.(*MetaNode)
		return metaNode.Total - metaNode.Used
	case MetaNodeRocksdb:
		metaNode := node.(*MetaNode)
		return metaNode.GetRocksdbTotal() - metaNode.GetRocksdbUsed()
	default:
		panic("unkown node type")
	}
}

func (s *AvailableSpaceFirstNodeSelector) GetName() string {
	return AvailableSpaceFirstNodeSelectorName
}

func (s *AvailableSpaceFirstNodeSelector) Select(ns *nodeSet, excludeHosts []string, replicaNum int) (newHosts []string, peers []proto.Peer, err error) {
	newHosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	// if replica == 0, return
	if replicaNum == 0 {
		return
	}
	orderHosts := make([]string, 0)
	nodes := ns.getNodes(s.nodeType)
	sortedNodes := make([]Node, 0)
	nodes.Range(func(key, value interface{}) bool {
		sortedNodes = append(sortedNodes, asNodeWrap(value, s.nodeType))
		return true
	})
	// if we cannot get enough nodes, return error
	if len(sortedNodes) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(sortedNodes))
		return
	}
	// sort nodes by available space
	sort.Slice(sortedNodes, func(i, j int) bool {
		return s.getNodeAvailableSpace(sortedNodes[i]) > s.getNodeAvailableSpace(sortedNodes[j])
	})
	nodeIndex := 0
	// pick first N nodes
	for i := 0; i < replicaNum && nodeIndex < len(sortedNodes); i++ {
		selectedIndex := len(sortedNodes)
		// loop until we get a writable node
		for nodeIndex < len(sortedNodes) {
			node := sortedNodes[nodeIndex]
			nodeIndex += 1
			if canAllocPartition(node, s.nodeType) {
				if excludeHosts == nil || !contains(excludeHosts, node.GetAddr()) {
					selectedIndex = nodeIndex - 1
					break
				}
			}
		}
		// if we get a writable node, append it to host list
		if selectedIndex != len(sortedNodes) {
			node := sortedNodes[selectedIndex]
			node.SelectNodeForWrite(s.nodeType)
			orderHosts = append(orderHosts, node.GetAddr())
			peer := proto.Peer{ID: node.GetID(), Addr: node.GetAddr()}
			peers = append(peers, peer)
		}
	}
	// if we cannot get enough writable nodes, return error
	if len(orderHosts) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough writable hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(orderHosts))
		return
	}
	log.LogInfof("action[%vNodeSelector::Select] peers[%v]", s.GetName(), peers)
	// reshuffle for primary-backup replication
	if newHosts, err = reshuffleHosts(orderHosts); err != nil {
		err = fmt.Errorf("action[%vNodeSelector::Select] err:%v  orderHosts is nil", s.GetName(), err.Error())
		return
	}
	return
}

func NewAvailableSpaceFirstNodeSelector(nodeType NodeResourceType) *AvailableSpaceFirstNodeSelector {
	return &AvailableSpaceFirstNodeSelector{
		nodeType: nodeType,
	}
}

type RoundRobinNodeSelector struct {
	index int

	nodeType NodeResourceType
}

func (s *RoundRobinNodeSelector) GetName() string {
	return RoundRobinNodeSelectorName
}

func (s *RoundRobinNodeSelector) Select(ns *nodeSet, excludeHosts []string, replicaNum int) (newHosts []string, peers []proto.Peer, err error) {
	newHosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	// if replica == 0, return
	if replicaNum == 0 {
		return
	}
	orderHosts := make([]string, 0)
	nodes := ns.getNodes(s.nodeType)
	sortedNodes := make([]Node, 0)
	nodes.Range(func(key, value interface{}) bool {
		sortedNodes = append(sortedNodes, asNodeWrap(value, s.nodeType))
		return true
	})
	// if we cannot get enough nodes, return error
	if len(sortedNodes) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough writable hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(sortedNodes))
		return
	}
	// sort nodes by id, so we can get a node list that is as stable as possible
	sort.Slice(sortedNodes, func(i, j int) bool {
		return sortedNodes[i].GetID() < sortedNodes[j].GetID()
	})
	nodeIndex := 0
	// pick first N nodes
	for i := 0; i < replicaNum && nodeIndex < len(sortedNodes); i++ {
		selectedIndex := len(sortedNodes)
		// loop until we get a writable node
		for nodeIndex < len(sortedNodes) {
			node := sortedNodes[(nodeIndex+s.index)%len(sortedNodes)]
			nodeIndex += 1
			if canAllocPartition(node, s.nodeType) {
				if excludeHosts == nil || !contains(excludeHosts, node.GetAddr()) {
					selectedIndex = nodeIndex - 1
					break
				}
			}
		}
		// if we get a writable node, append it to host list
		if selectedIndex != len(sortedNodes) {
			node := sortedNodes[(selectedIndex+s.index)%len(sortedNodes)]
			orderHosts = append(orderHosts, node.GetAddr())
			node.SelectNodeForWrite(s.nodeType)
			peer := proto.Peer{ID: node.GetID(), Addr: node.GetAddr()}
			peers = append(peers, peer)
		}
	}
	// if we cannot get enough writable nodes, return error
	if len(orderHosts) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough writable hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(orderHosts))
		return
	}
	// move the index of selector
	s.index += nodeIndex
	log.LogInfof("action[%vNodeSelector::Select] peers[%v]", s.GetName(), peers)
	// reshuffle for primary-backup replication
	if newHosts, err = reshuffleHosts(orderHosts); err != nil {
		err = fmt.Errorf("action[%vNodeSelector::Select] err:%v  orderHosts is nil", s.GetName(), err.Error())
		return
	}
	return
}

func NewRoundRobinNodeSelector(nodeType NodeResourceType) *RoundRobinNodeSelector {
	return &RoundRobinNodeSelector{
		nodeType: nodeType,
	}
}

const (
	StrawNodeSelectorRandMax = 65536
)

// NOTE: this node selector inspired by Straw2 algorithm, which is widely used in ceph
type StrawNodeSelector struct {
	rand     *rand.Rand
	nodeType NodeResourceType
}

func (s *StrawNodeSelector) GetName() string {
	return StrawNodeSelectorName
}

func (s *StrawNodeSelector) getWeight(node Node) float64 {
	switch s.nodeType {
	case DataNodeDisk:
		dataNode := node.(*DataNode)
		return float64(dataNode.AvailableSpace) / util.GB
	case MetaNodeMemory:
		metaNode := node.(*MetaNode)
		return float64(metaNode.Total-metaNode.Used) / util.GB
	case MetaNodeRocksdb:
		metaNode := node.(*MetaNode)
		return float64(metaNode.GetRocksdbTotal()-metaNode.GetRocksdbUsed()) / util.GB
	default:
		panic("unkown node type")
	}
}

func (s *StrawNodeSelector) selectOneNode(nodes []Node) (index int, maxNode Node) {
	maxStraw := float64(0)
	index = -1
	for i, node := range nodes {
		straw := float64(s.rand.Intn(StrawNodeSelectorRandMax))
		straw = math.Log(straw/float64(StrawNodeSelectorRandMax)) / s.getWeight(node)
		if index == -1 || straw > maxStraw {
			maxStraw = straw
			maxNode = node
			index = i
		}
	}
	return
}

func (s *StrawNodeSelector) Select(ns *nodeSet, excludeHosts []string, replicaNum int) (newHosts []string, peers []proto.Peer, err error) {
	nodes := make([]Node, 0)
	ns.getNodes(s.nodeType).Range(func(key, value interface{}) bool {
		node := asNodeWrap(value, s.nodeType)
		if !contains(excludeHosts, node.GetAddr()) {
			nodes = append(nodes, node)
		}
		return true
	})
	orderHosts := make([]string, 0)
	for len(orderHosts) < replicaNum {
		if len(nodes)+len(orderHosts) < replicaNum {
			break
		}
		index, node := s.selectOneNode(nodes)
		if index != 0 {
			nodes[0], nodes[index] = node, nodes[0]
		}
		nodes = nodes[1:]
		if !canAllocPartition(node, s.nodeType) {
			continue
		}
		orderHosts = append(orderHosts, node.GetAddr())
		node.SelectNodeForWrite(s.nodeType)
		peer := proto.Peer{ID: node.GetID(), Addr: node.GetAddr()}
		peers = append(peers, peer)
	}
	// if we cannot get enough writable nodes, return error
	if len(orderHosts) < replicaNum {
		err = fmt.Errorf("action[%vNodeSelector::Select] no enough writable hosts,replicaNum:%v  MatchNodeCount:%v  ",
			s.GetName(), replicaNum, len(orderHosts))
		return
	}
	log.LogInfof("action[%vNodeSelector::Select] peers[%v]", s.GetName(), peers)
	// reshuffle for primary-backup replication
	if newHosts, err = reshuffleHosts(orderHosts); err != nil {
		err = fmt.Errorf("action[%vNodeSelector::Select] err:%v  orderHosts is nil", s.GetName(), err.Error())
		return
	}
	return
}

func NewStrawNodeSelector(nodeType NodeResourceType) *StrawNodeSelector {
	return &StrawNodeSelector{
		rand:     rand.New(rand.NewSource(time.Now().UnixMicro())),
		nodeType: nodeType,
	}
}

func NewNodeSelector(name string, nodeType NodeResourceType) NodeSelector {
	switch name {
	case RoundRobinNodeSelectorName:
		return NewRoundRobinNodeSelector(nodeType)
	case CarryWeightNodeSelectorName:
		return NewCarryWeightNodeSelector(nodeType)
	case AvailableSpaceFirstNodeSelectorName:
		return NewAvailableSpaceFirstNodeSelector(nodeType)
	case StrawNodeSelectorName:
		return NewStrawNodeSelector(nodeType)
	default:
		return NewCarryWeightNodeSelector(nodeType)
	}
}

func (ns *nodeSet) getAvailMetaNodeHosts(excludeHosts []string, replicaNum int, storeMode proto.StoreMode) (newHosts []string, peers []proto.Peer, err error) {
	ns.nodeSelectLock.Lock()
	defer ns.nodeSelectLock.Unlock()
	// we need a read lock to block the modify of node selector
	ns.metaNodeSelectorLock.RLock()
	defer ns.metaNodeSelectorLock.RUnlock()
	if storeMode == proto.StoreModeRocksDb {
		return ns.metaNodeRocksdbSelector.Select(ns, excludeHosts, replicaNum)
	}
	return ns.metaNodeMemorySelector.Select(ns, excludeHosts, replicaNum)
}

func (ns *nodeSet) getAvailDataNodeHosts(excludeHosts []string, replicaNum int) (hosts []string, peers []proto.Peer, err error) {
	ns.nodeSelectLock.Lock()
	defer ns.nodeSelectLock.Unlock()
	// we need a read lock to block the modify of node selector
	ns.dataNodeSelectorLock.Lock()
	defer ns.dataNodeSelectorLock.Unlock()
	return ns.dataNodeDiskSelector.Select(ns, excludeHosts, replicaNum)
}
