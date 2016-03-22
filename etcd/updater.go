// Updater of Go "flags"-compatible data base on dynamic etcd watches.
//
// Copyright 2015 Michal Witkowski. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package etcd provides an updater for go "flags"-compatible FlagSets based on dynamic changes in etcd storage.

package etcd

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	etcd "github.com/coreos/etcd/client"
	"github.com/spf13/pflag"
	"github.com/mwitkow/go-flagz"
)


var(
	errNoValue = fmt.Errorf("no value in Node")
	errFlagNotDynamic = fmt.Errorf("flag is not dynamic")
)

// Controls the auto updating process of a "flags"-compatible package from Etcd.
type Updater struct {
	client    etcd.Client
	etcdKeys  etcd.KeysAPI
	flagSet   *pflag.FlagSet
	logger    logger
	etcdPath  string
	lastIndex uint64
	watching  bool
	context   context.Context
	cancel    context.CancelFunc
}

// Minimum logger interface needed.
// Default "log" and "logrus" should support these.
type logger interface {
	Printf(format string, v ...interface{})
}

func New(set *pflag.FlagSet, keysApi etcd.KeysAPI, etcdPath string, logger logger) (*Updater, error) {
	if !strings.HasSuffix(etcdPath, "/") {
		etcdPath = etcdPath + "/"
	}
	u := &Updater{
		flagSet:   set,
		etcdKeys:  keysApi,
		etcdPath:  etcdPath,
		logger:    logger,
		lastIndex: 0,
		watching:  false,
	}
	u.context, u.cancel = context.WithCancel(context.Background())
	return u, nil
}

// Performs the initial read of etcd for all flags and updates the specified FlagSet.
func (u *Updater) Initialize() error {
	if u.lastIndex != 0 {
		return fmt.Errorf("flagz: already initialized.")
	}
	return u.readAllFlags(/* onlyDynamic */ false)
}

// Starts the auto-updating go-routine.
func (u *Updater) Start() error {
	if u.lastIndex == 0 {
		return fmt.Errorf("flagz: not initialized")
	}
	if u.watching {
		return fmt.Errorf("flagz: already watching")
	}
	u.watching = true
	go u.watchForUpdates()
	return nil
}

// Stops the auto-updating go-routine.
func (u *Updater) Stop() error {
	if !u.watching {
		return fmt.Errorf("flagz: not watching")
	}
	u.logger.Printf("flagz: stopping")
	u.cancel()
	return nil
}

func (u *Updater) readAllFlags(onlyDynamic bool) error {
	resp, err := u.etcdKeys.Get(u.context, u.etcdPath, &etcd.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		return err
	}
	u.lastIndex = resp.Index
	errorStrings := []string{}
	for _, node := range resp.Node.Nodes {
		flagName, err := u.nodeToFlagName(node)
		if err != nil {
			u.logger.Printf("flagz: ignoring: %v", err)
			continue
		}
		if err := u.setFlag(flagName, node.Value, onlyDynamic); err != nil && err != errNoValue {
			errorStrings = append(errorStrings, err.Error())
		}
	}
	if len(errorStrings) > 0 {
		return fmt.Errorf("flagz: encountered %d errors while parsing flags from etcd: \n  %v",
			len(errorStrings), strings.Join(errorStrings, "\n"))
	}
	return nil
}

func (u *Updater) setFlag(flagName string, value string, onlyDynamic bool) error {
	if value == "" {
		return errNoValue
	}
	flag := u.flagSet.Lookup(flagName)
	if flag == nil {
		return fmt.Errorf("flag=%v was not found", flagName)
	}
	if onlyDynamic && !flagz.IsFlagDynamic(flag) {
		return errFlagNotDynamic
	}
	return flag.Value.Set(value)
}

func (u *Updater) watchForUpdates() error {
	// We need to implement our own watcher because the one in go-etcd doesn't handle errorcode 400 and 401.
	// See https://github.com/coreos/etcd/blob/master/Documentation/errorcode.md
	// And https://coreos.com/etcd/docs/2.0.8/api.html#waiting-for-a-change
	watcher := u.etcdKeys.Watcher(u.etcdPath, &etcd.WatcherOptions{AfterIndex: u.lastIndex, Recursive: true})
	u.logger.Printf("flagz: watcher started")
	for u.watching {
		resp, err := watcher.Next(u.context)
		if etcdErr, ok := err.(etcd.Error); ok && etcdErr.Code == etcd.ErrorCodeEventIndexCleared {
			// Our index is out of the Etcd Log. Reread everything and reset index.
			u.logger.Printf("flagz: handling Etcd Index error by re-reading everything: %v", err)
			time.Sleep(200 * time.Millisecond)
			u.readAllFlags(/* onlyDynamic */ true)
			watcher = u.etcdKeys.Watcher(u.etcdPath, &etcd.WatcherOptions{AfterIndex: u.lastIndex, Recursive: true})
			continue
		} else if clusterErr, ok := err.(*etcd.ClusterError); ok {
			u.logger.Printf("flagz: etcd ClusterError. Will retry. %v", clusterErr.Detail())
			time.Sleep(100 * time.Millisecond)
			continue
		} else if err == context.DeadlineExceeded {
			u.logger.Printf("flagz: deadline exceeded which watching for changes, continuing watching")
			continue
		} else if err == context.Canceled {
			break
		} else if err != nil {
			u.logger.Printf("flagz: wicked etcd error. Restarting watching after some time. %v", err)
			// Etcd started dropping watchers, or is re-electing. Give it some time.
			randOffsetMs := int(500 * rand.Float32())
			time.Sleep(1*time.Second + time.Duration(randOffsetMs)*time.Millisecond)
			continue
		}
		u.lastIndex = resp.Node.ModifiedIndex
		flagName, err := u.nodeToFlagName(resp.Node)
		if err != nil {
			u.logger.Printf("flagz: ignoring %v at etcdindex=%v", err, u.lastIndex)
			continue
		}
		err = u.setFlag(flagName, resp.Node.Value, /*onlyDynamic*/ true)
		if err == errNoValue {
			u.logger.Printf("flagz: ignoring action=%v on flag=%v at etcdindex=%v", resp.Action, flagName, u.lastIndex)
			continue
		} else if err == errFlagNotDynamic {
			u.logger.Printf("flagz: ignoring updating flag=%v at etcdindex=%v, because of: %v", flagName, u.lastIndex, err)
		} else if err != nil {
			u.logger.Printf("flagz: failed updating flag=%v at etcdindex=%v, because of: %v", flagName, u.lastIndex, err)
			u.rollbackEtcdValue(flagName, resp)
		} else {
			u.logger.Printf("flagz: updated flag=%v to value=%v at etcdindex=%v", flagName, resp.Node.Value, u.lastIndex)
		}
	}
	u.logger.Printf("flagz: watcher exited")
	return nil
}

func (u *Updater) rollbackEtcdValue(flagName string, resp *etcd.Response) {
	var err error
	if resp.PrevNode != nil {
		// It's just a new value that's wrong, roll back to prevNode value atomically.
		_, err = u.etcdKeys.Set(u.context, resp.Node.Key, resp.PrevNode.Value, &etcd.SetOptions{PrevIndex: u.lastIndex})
	} else {
		_, err = u.etcdKeys.Delete(u.context, resp.Node.Key, &etcd.DeleteOptions{PrevIndex: u.lastIndex})
	}
	if etcdErr, ok := err.(etcd.Error); ok && etcdErr.Code == etcd.ErrorCodeTestFailed {
		// Someone probably rolled it back in the mean time.
		u.logger.Printf("flagz: rolled back flag=%v was changed by someone else. All good.", flagName)
	} else if err != nil {
		u.logger.Printf("flagz: rolling back flagz=%v failed: %v", flagName, err)
	} else {
		u.logger.Printf("flagz: rolled back flagz=%v to correct state. All good.", flagName)
	}
}


func (u *Updater) nodeToFlagName(node *etcd.Node) (string, error) {
	if node.Dir {
		return "", fmt.Errorf("key '%v' is a directory entry", node.Key)
	}
	if !strings.HasPrefix(node.Key, u.etcdPath) {
		return "", fmt.Errorf("key '%v' doesn't start with etcd path '%v'", node.Key, u.etcdPath)
	}
	truncated := strings.TrimPrefix(node.Key, u.etcdPath)
	if strings.Count(truncated, "/") > 0 {
		return "", fmt.Errorf("key '%v' isn't a direct leaf of etcd path '%v'", node.Key, u.etcdPath)
	}
	return truncated, nil
}

