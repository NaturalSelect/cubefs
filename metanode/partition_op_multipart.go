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

package metanode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cubefs/cubefs/util/exporter"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
)

func (mp *metaPartition) GetExpiredMultipart(req *proto.GetExpiredMultipartRequest, p *Packet) (err error) {
	expiredMultiPartInfos := make([]*proto.ExpiredMultipartInfo, 0)
	walkTreeFunc := func(multipart *Multipart) (bool, error) {
		if len(req.Prefix) > 0 && !strings.HasPrefix(multipart.key, req.Prefix) {
			// skip and continue
			return true, nil
		}

		if multipart.initTime.Unix()+int64(req.Days*24*60*60) <= time.Now().Local().Unix() {
			info := &proto.ExpiredMultipartInfo{
				Path:        multipart.key,
				MultipartId: multipart.id,
				Inodes:      make([]uint64, 0),
			}
			for _, part := range multipart.Parts() {
				info.Inodes = append(info.Inodes, part.Inode)
			}
			expiredMultiPartInfos = append(expiredMultiPartInfos, info)
		}

		return true, nil
	}

	mp.multipartTree.Range(nil, nil, walkTreeFunc)

	resp := &proto.GetExpiredMultipartResponse{
		Infos: expiredMultiPartInfos,
	}

	var reply []byte
	if reply, err = json.Marshal(resp); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	p.PacketOkWithBody(reply)
	return
}

func (mp *metaPartition) GetMultipart(req *proto.GetMultipartRequest, p *Packet) (err error) {
	var multipart *Multipart
	multipart, err = mp.multipartTree.RefGet(req.Path, req.MultipartId)
	if err != nil {
		if err == ErrRocksdbOperation {
			exporter.WarningRocksdbError(fmt.Sprintf("action[GetMultipart] clusterID[%s] volumeName[%s] partitionID[%v]"+
				" get multipart failed witch rocksdb error[multipart path:%s, id:%s]", mp.manager.metaNode.clusterId, mp.config.VolName,
				mp.config.PartitionId, req.Path, req.MultipartId))
		}
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	if multipart == nil {
		p.PacketErrorWithBody(proto.OpNotExistErr, nil)
		return
	}

	resp := &proto.GetMultipartResponse{
		Info: &proto.MultipartInfo{
			ID:       multipart.id,
			Path:     multipart.key,
			InitTime: multipart.initTime,
			Parts:    make([]*proto.MultipartPartInfo, 0, len(multipart.parts)),
			Extend:   multipart.extend,
		},
	}
	for _, part := range multipart.Parts() {
		resp.Info.Parts = append(resp.Info.Parts, &proto.MultipartPartInfo{
			ID:         part.ID,
			Inode:      part.Inode,
			MD5:        part.MD5,
			Size:       part.Size,
			UploadTime: part.UploadTime,
		})
	}
	var reply []byte
	if reply, err = json.Marshal(resp); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	p.PacketOkWithBody(reply)
	return
}

func (mp *metaPartition) AppendMultipart(req *proto.AddMultipartPartRequest, p *Packet) (err error) {
	if req.Part == nil {
		p.PacketOkReply()
		return
	}
	var multipart *Multipart
	multipart, err = mp.multipartTree.RefGet(req.Path, req.MultipartId)
	if err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	if multipart == nil {
		p.PacketErrorWithBody(proto.OpNotExistErr, nil)
		return
	}
	multipartAppend := &Multipart{
		id:  req.MultipartId,
		key: req.Path,
		parts: Parts{
			&Part{
				ID:         req.Part.ID,
				UploadTime: req.Part.UploadTime,
				MD5:        req.Part.MD5,
				Size:       req.Part.Size,
				Inode:      req.Part.Inode,
			},
		},
	}
	var resp interface{}
	if resp, err = mp.putMultipart(opFSMAppendMultipart, multipartAppend); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	appendMultipartResp := resp.(proto.AppendMultipartResponse)
	if appendMultipartResp.Status != proto.OpOk {
		p.PacketErrorWithBody(appendMultipartResp.Status, nil)
		return
	}
	var reply []byte
	if reply, err = json.Marshal(appendMultipartResp); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	p.PacketOkWithBody(reply)
	return
}

func (mp *metaPartition) RemoveMultipart(req *proto.RemoveMultipartRequest, p *Packet) (err error) {
	multipart := &Multipart{
		id:  req.MultipartId,
		key: req.Path,
	}
	var resp interface{}
	if resp, err = mp.putMultipart(opFSMRemoveMultipart, multipart); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	status := resp.(uint8)
	if status != proto.OpOk {
		p.PacketErrorWithBody(status, nil)
		return
	}
	p.PacketOkReply()
	return
}

func (mp *metaPartition) CreateMultipart(req *proto.CreateMultipartRequest, p *Packet) (err error) {
	var (
		multipartId     string
		storedMultipart *Multipart
	)
	for {
		multipartId = util.CreateMultipartID(mp.config.PartitionId).String()
		storedMultipart, err = mp.multipartTree.RefGet(req.Path, multipartId)
		if err != nil {
			p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
			return
		}
		if storedMultipart == nil {
			break
		}
	}

	multipart := &Multipart{
		id:       multipartId,
		key:      req.Path,
		initTime: time.Now().Local(),
		extend:   req.Extend,
	}
	if _, err = mp.putMultipart(opFSMCreateMultipart, multipart); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}

	resp := &proto.CreateMultipartResponse{
		Info: &proto.MultipartInfo{
			ID:   multipartId,
			Path: req.Path,
		},
	}
	var reply []byte
	if reply, err = json.Marshal(resp); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	p.PacketOkWithBody(reply)
	return
}

func (mp *metaPartition) ListMultipart(req *proto.ListMultipartRequest, p *Packet) (err error) {
	max := int(req.Max)
	keyMarker := req.Marker
	multipartIdMarker := req.MultipartIdMarker
	prefix := req.Prefix
	matches := make([]*Multipart, 0, max)
	walkTreeFunc := func(multipart *Multipart) (bool, error) {
		if len(prefix) > 0 && !strings.HasPrefix(multipart.key, prefix) {
			// skip and continue
			return true, nil
		}
		matches = append(matches, multipart.Copy().(*Multipart))
		return !(len(matches) >= max), nil
	}
	if len(keyMarker) > 0 {
		err = mp.multipartTree.Range(&Multipart{key: keyMarker, id: multipartIdMarker}, nil, walkTreeFunc)
	} else {
		err = mp.multipartTree.Range(nil, nil, walkTreeFunc)
	}
	if err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	multipartInfos := make([]*proto.MultipartInfo, len(matches))

	convertPartFunc := func(part *Part) *proto.MultipartPartInfo {
		return &proto.MultipartPartInfo{
			ID:         part.ID,
			Inode:      part.Inode,
			MD5:        part.MD5,
			Size:       part.Size,
			UploadTime: part.UploadTime,
		}
	}

	convertMultipartFunc := func(multipart *Multipart) *proto.MultipartInfo {
		partInfos := make([]*proto.MultipartPartInfo, len(multipart.parts))
		for i := 0; i < len(multipart.parts); i++ {
			partInfos[i] = convertPartFunc(multipart.parts[i])
		}
		return &proto.MultipartInfo{
			ID:       multipart.id,
			Path:     multipart.key,
			InitTime: multipart.initTime,
			Parts:    partInfos,
		}
	}

	for i := 0; i < len(matches); i++ {
		multipartInfos[i] = convertMultipartFunc(matches[i])
	}

	resp := &proto.ListMultipartResponse{
		Multiparts: multipartInfos,
	}

	var reply []byte
	if reply, err = json.Marshal(resp); err != nil {
		p.PacketErrorWithBody(proto.OpErr, []byte(err.Error()))
		return
	}
	p.PacketOkWithBody(reply)
	return
}

// SendMultipart replicate specified multipart operation to raft.
func (mp *metaPartition) putMultipart(op uint32, multipart *Multipart) (resp interface{}, err error) {
	var encoded []byte
	if encoded, err = multipart.Bytes(); err != nil {
		return
	}
	resp, err = mp.submit(op, encoded)
	return
}
