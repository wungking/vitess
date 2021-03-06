// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Actions modify the state of a tablet, shard or keyspace.
//
// They are stored in topology server and form a queue. Only the
// lowest action id should be executing at any given time.
//
// The creation, deletion and modifaction of an action node may be used as
// a signal to other components in the system.

package tabletmanager

import (
	"fmt"
	"os"
	"os/user"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/hook"
	"github.com/youtube/vitess/go/vt/key"
	"github.com/youtube/vitess/go/vt/mysqlctl"
	"github.com/youtube/vitess/go/vt/topo"
)

// The actor applies individual commands to execute an action read from a node
// in topology server.
//
// The actor signals completion by removing the action node from the topology server.
//
// Errors are written to the action node and must (currently) be resolved by
// hand using zk tools.

var interrupted = make(chan struct{})
var once sync.Once

// In certain cases (vtctl most notably) having SIGINT manifest itself
// as an instant timeout lets us break out cleanly.
func SignalInterrupt() {
	close(interrupted)
}

type InitiatorError string

func (e InitiatorError) Error() string {
	return string(e)
}

type ActionInitiator struct {
	ts  topo.Server
	rpc TabletManagerConn
}

func NewActionInitiator(ts topo.Server, tabletManagerProtocol string) *ActionInitiator {
	f, ok := tabletManagerConnFactories[tabletManagerProtocol]
	if !ok {
		log.Fatalf("No TabletManagerProtocol registered with name %s", tabletManagerProtocol)
	}

	return &ActionInitiator{ts, f(ts)}
}

func actionGuid() string {
	now := time.Now().Format(time.RFC3339)
	username := "unknown"
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	hostname := "unknown"
	if h, err := os.Hostname(); err == nil {
		hostname = h
	}
	return fmt.Sprintf("%v-%v-%v", now, username, hostname)
}

func (ai *ActionInitiator) writeTabletAction(tabletAlias topo.TabletAlias, node *ActionNode) (actionPath string, err error) {
	node.ActionGuid = actionGuid()
	data := ActionNodeToJson(node)
	return ai.ts.WriteTabletAction(tabletAlias, data)
}

func (ai *ActionInitiator) Ping(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_PING})
}

func (ai *ActionInitiator) RpcPing(tabletAlias topo.TabletAlias, waitTime time.Duration) error {
	tablet, err := ai.ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}

	return ai.rpc.Ping(tablet, waitTime)
}

func (ai *ActionInitiator) Sleep(tabletAlias topo.TabletAlias, duration time.Duration) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SLEEP, args: &duration})
}

func (ai *ActionInitiator) ChangeType(tabletAlias topo.TabletAlias, dbType topo.TabletType) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_CHANGE_TYPE, args: &dbType})
}

func (ai *ActionInitiator) RpcChangeType(tablet *topo.TabletInfo, dbType topo.TabletType, waitTime time.Duration) error {
	return ai.rpc.ChangeType(tablet, dbType, waitTime)
}

func (ai *ActionInitiator) SetReadOnly(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SET_RDONLY})
}

func (ai *ActionInitiator) SetReadWrite(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SET_RDWR})
}

func (ai *ActionInitiator) DemoteMaster(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_DEMOTE_MASTER})
}

type SnapshotArgs struct {
	Concurrency int
	ServerMode  bool
}

type SnapshotReply struct {
	ParentAlias  topo.TabletAlias
	ManifestPath string

	// these two are only used for ServerMode=true full snapshot
	SlaveStartRequired bool
	ReadOnly           bool
}

type MultiSnapshotReply struct {
	ParentAlias   topo.TabletAlias
	ManifestPaths []string
}

func (ai *ActionInitiator) Snapshot(tabletAlias topo.TabletAlias, args *SnapshotArgs) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SNAPSHOT, args: args})
}

type SnapshotSourceEndArgs struct {
	SlaveStartRequired bool
	ReadOnly           bool
}

func (ai *ActionInitiator) SnapshotSourceEnd(tabletAlias topo.TabletAlias, args *SnapshotSourceEndArgs) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SNAPSHOT_SOURCE_END, args: args})
}

type MultiSnapshotArgs struct {
	KeyName          string
	KeyRanges        []key.KeyRange
	Tables           []string
	Concurrency      int
	SkipSlaveRestart bool
	MaximumFilesize  uint64
}

type MultiRestoreArgs struct {
	SrcTabletAliases       []topo.TabletAlias
	Concurrency            int
	FetchConcurrency       int
	InsertTableConcurrency int
	FetchRetryCount        int
	Strategy               string
}

func (ai *ActionInitiator) MultiSnapshot(tabletAlias topo.TabletAlias, args *MultiSnapshotArgs) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_MULTI_SNAPSHOT, args: args})
}

func (ai *ActionInitiator) MultiRestore(tabletAlias topo.TabletAlias, args *MultiRestoreArgs) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_MULTI_RESTORE, args: args})
}

func (ai *ActionInitiator) BreakSlaves(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_BREAK_SLAVES})
}

func (ai *ActionInitiator) PromoteSlave(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_PROMOTE_SLAVE})
}

func (ai *ActionInitiator) SlaveWasPromoted(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SLAVE_WAS_PROMOTED})
}

func (ai *ActionInitiator) RpcSlaveWasPromoted(tablet *topo.TabletInfo, waitTime time.Duration) error {
	return ai.rpc.SlaveWasPromoted(tablet, waitTime)
}

func (ai *ActionInitiator) RestartSlave(tabletAlias topo.TabletAlias, args *RestartSlaveData) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_RESTART_SLAVE, args: args})
}

type SlaveWasRestartedData struct {
	Parent               topo.TabletAlias
	ExpectedMasterAddr   string
	ExpectedMasterIpAddr string
	ScrapStragglers      bool
}

func (ai *ActionInitiator) SlaveWasRestarted(tabletAlias topo.TabletAlias, args *SlaveWasRestartedData) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SLAVE_WAS_RESTARTED, args: args})
}

func (ai *ActionInitiator) RpcSlaveWasRestarted(tablet *topo.TabletInfo, args *SlaveWasRestartedData, waitTime time.Duration) error {
	return ai.rpc.SlaveWasRestarted(tablet, args, waitTime)
}

func (ai *ActionInitiator) ReparentPosition(tabletAlias topo.TabletAlias, slavePos *mysqlctl.ReplicationPosition) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_REPARENT_POSITION, args: slavePos})
}

func (ai *ActionInitiator) MasterPosition(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_MASTER_POSITION})
}

func (ai *ActionInitiator) SlavePosition(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SLAVE_POSITION})
}

type SlavePositionReq struct {
	ReplicationPosition mysqlctl.ReplicationPosition
	WaitTimeout         int // seconds, zero to wait indefinitely
}

func (ai *ActionInitiator) WaitSlavePosition(tabletAlias topo.TabletAlias, args *SlavePositionReq) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_WAIT_SLAVE_POSITION, args: args})
}

func (ai *ActionInitiator) StopSlave(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_STOP_SLAVE})
}

func (ai *ActionInitiator) WaitBlpPosition(tabletAlias topo.TabletAlias, blpPosition mysqlctl.BlpPosition, waitTime time.Duration) error {
	tablet, err := ai.ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}

	return ai.rpc.WaitBlpPosition(tablet, blpPosition, waitTime)
}

type ReserveForRestoreArgs struct {
	SrcTabletAlias topo.TabletAlias
}

func (ai *ActionInitiator) ReserveForRestore(dstTabletAlias topo.TabletAlias, args *ReserveForRestoreArgs) (actionPath string, err error) {
	return ai.writeTabletAction(dstTabletAlias, &ActionNode{Action: TABLET_ACTION_RESERVE_FOR_RESTORE, args: args})
}

type RestoreArgs struct {
	SrcTabletAlias        topo.TabletAlias
	SrcFilePath           string
	ParentAlias           topo.TabletAlias
	FetchConcurrency      int
	FetchRetryCount       int
	WasReserved           bool
	DontWaitForSlaveStart bool
}

func (ai *ActionInitiator) Restore(dstTabletAlias topo.TabletAlias, args *RestoreArgs) (actionPath string, err error) {
	return ai.writeTabletAction(dstTabletAlias, &ActionNode{Action: TABLET_ACTION_RESTORE, args: args})
}

func (ai *ActionInitiator) Scrap(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_SCRAP})
}

type GetSchemaArgs struct {
	Tables       []string
	IncludeViews bool
}

func (ai *ActionInitiator) GetSchema(tabletAlias topo.TabletAlias, tables []string, includeViews bool) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_GET_SCHEMA, args: &GetSchemaArgs{Tables: tables, IncludeViews: includeViews}})
}

func (ai *ActionInitiator) RpcGetSchemaTablet(tablet *topo.TabletInfo, tables []string, includeViews bool, waitTime time.Duration) (*mysqlctl.SchemaDefinition, error) {
	return ai.rpc.GetSchemaTablet(tablet, tables, includeViews, waitTime)
}

func (ai *ActionInitiator) PreflightSchema(tabletAlias topo.TabletAlias, change string) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_PREFLIGHT_SCHEMA, args: &change})
}

func (ai *ActionInitiator) ApplySchema(tabletAlias topo.TabletAlias, sc *mysqlctl.SchemaChange) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_APPLY_SCHEMA, args: sc})
}

func (ai *ActionInitiator) RpcGetPermissions(tabletAlias topo.TabletAlias, waitTime time.Duration) (*mysqlctl.Permissions, error) {
	tablet, err := ai.ts.GetTablet(tabletAlias)
	if err != nil {
		return nil, err
	}

	return ai.rpc.GetPermissions(tablet, waitTime)
}

func (ai *ActionInitiator) ExecuteHook(tabletAlias topo.TabletAlias, _hook *hook.Hook) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_EXECUTE_HOOK, args: _hook})
}

type SlaveList struct {
	Addrs []string
}

func (ai *ActionInitiator) GetSlaves(tabletAlias topo.TabletAlias) (actionPath string, err error) {
	return ai.writeTabletAction(tabletAlias, &ActionNode{Action: TABLET_ACTION_GET_SLAVES})
}

func (ai *ActionInitiator) ReparentShard(tabletAlias topo.TabletAlias) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_REPARENT,
		ActionGuid: actionGuid(),
		args:       &tabletAlias,
	}
}

func (ai *ActionInitiator) ShardExternallyReparented(tabletAlias topo.TabletAlias) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_EXTERNALLY_REPARENTED,
		ActionGuid: actionGuid(),
		args:       &tabletAlias,
	}
}

func (ai *ActionInitiator) RebuildShard() *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_REBUILD,
		ActionGuid: actionGuid(),
	}
}

func (ai *ActionInitiator) CheckShard() *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_CHECK,
		ActionGuid: actionGuid(),
	}
}

// parameters are stored for debug purposes
type ApplySchemaShardArgs struct {
	MasterTabletAlias topo.TabletAlias
	Change            string
	Simple            bool
}

func (ai *ActionInitiator) ApplySchemaShard(masterTabletAlias topo.TabletAlias, change string, simple bool) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_APPLY_SCHEMA,
		ActionGuid: actionGuid(),
		args: &ApplySchemaShardArgs{
			MasterTabletAlias: masterTabletAlias,
			Change:            change,
			Simple:            simple,
		},
	}
}

// parameters are stored for debug purposes
type SetShardServedTypesArgs struct {
	ServedTypes []topo.TabletType
}

func (ai *ActionInitiator) SetShardServedTypes(servedTypes []topo.TabletType) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_SET_SERVED_TYPES,
		ActionGuid: actionGuid(),
		args: &SetShardServedTypesArgs{
			ServedTypes: servedTypes,
		},
	}
}

func (ai *ActionInitiator) ShardMultiRestore(args *MultiRestoreArgs) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_MULTI_RESTORE,
		ActionGuid: actionGuid(),
		args:       args,
	}
}

// parameters are stored for debug purposes
type MigrateServedTypesArgs struct {
	ServedType topo.TabletType
}

func (ai *ActionInitiator) MigrateServedTypes(servedType topo.TabletType) *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_MIGRATE_SERVED_TYPES,
		ActionGuid: actionGuid(),
		args: &MigrateServedTypesArgs{
			ServedType: servedType,
		},
	}
}

func (ai *ActionInitiator) UpdateShard() *ActionNode {
	return &ActionNode{
		Action:     SHARD_ACTION_UPDATE_SHARD,
		ActionGuid: actionGuid(),
	}
}

func (ai *ActionInitiator) RebuildKeyspace() *ActionNode {
	return &ActionNode{
		Action:     KEYSPACE_ACTION_REBUILD,
		ActionGuid: actionGuid(),
	}
}

// parameters are stored for debug purposes
type ApplySchemaKeyspaceArgs struct {
	Change string
	Simple bool
}

func (ai *ActionInitiator) ApplySchemaKeyspace(change string, simple bool) *ActionNode {
	return &ActionNode{
		Action:     KEYSPACE_ACTION_APPLY_SCHEMA,
		ActionGuid: actionGuid(),
		args: &ApplySchemaKeyspaceArgs{
			Change: change,
			Simple: simple,
		},
	}
}

func (ai *ActionInitiator) WaitForCompletion(actionPath string, waitTime time.Duration) error {
	_, err := WaitForCompletion(ai.ts, actionPath, waitTime)
	return err
}

func (ai *ActionInitiator) WaitForCompletionReply(actionPath string, waitTime time.Duration) (interface{}, error) {
	return WaitForCompletion(ai.ts, actionPath, waitTime)
}

func WaitForCompletion(ts topo.Server, actionPath string, waitTime time.Duration) (interface{}, error) {
	// If there is no duration specified, block for a sufficiently long time.
	if waitTime <= 0 {
		waitTime = 24 * time.Hour
	}

	data, err := ts.WaitForTabletAction(actionPath, waitTime, interrupted)
	if err != nil {
		return nil, err
	}

	// parse it
	actionNode, dataErr := ActionNodeFromJson(data, "")
	if dataErr != nil {
		return nil, fmt.Errorf("action data error: %v %v %#v", actionPath, dataErr, data)
	} else if actionNode.Error != "" {
		return nil, fmt.Errorf("action failed: %v %v", actionPath, actionNode.Error)
	}

	return actionNode.reply, nil
}
