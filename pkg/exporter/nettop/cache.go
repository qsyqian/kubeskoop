package nettop

import (
	"context"
	"fmt"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/vishvananda/netns"
	"golang.org/x/exp/slog"
)

const (
	hostNetwork   = "hostNetwork"
	unknowNetwork = "unknow"
)

var (
	cacheUpdateInterval = 10 * time.Second
	podCache            = cache.New(20*cacheUpdateInterval, 20*cacheUpdateInterval)
	nsCache             = cache.New(20*cacheUpdateInterval, 20*cacheUpdateInterval)
	pidCache            = cache.New(20*cacheUpdateInterval, 20*cacheUpdateInterval)

	control = make(chan struct{})
)

type netnsMeta struct {
	inum          int
	mountPath     string
	pids          []int
	isHostNetwork bool
}

type podMeta struct {
	name      string
	namespace string
	sandbox   string
	pid       int
	nspath    string
	app       string // app label from cri response
	ip        string // ip addr from cri response
	labels    map[string]string
}

type Entity struct {
	netnsMeta
	podMeta
	pids []int
}

func (e *Entity) GetIP() string {
	return e.podMeta.ip
}

func (e *Entity) GetAppLabel() string {
	if e.netnsMeta.isHostNetwork {
		return hostNetwork
	}
	return e.podMeta.app
}

func (e *Entity) GetLabel(labelkey string) (string, bool) {
	if e.podMeta.labels != nil {
		if v, ok := e.podMeta.labels[labelkey]; ok {
			return v, true
		}
	}

	return "", false
}

func (e *Entity) GetPodName() string {
	if e.netnsMeta.isHostNetwork {
		return hostNetwork
	}

	if e.podMeta.name != "" {
		return e.podMeta.name
	}

	return unknowNetwork
}

func (e *Entity) GetPodNamespace() string {
	if e.netnsMeta.isHostNetwork {
		return hostNetwork
	}

	if e.podMeta.namespace != "" {
		return e.podMeta.namespace
	}

	return unknowNetwork
}

func (e *Entity) GetMeta(name string) (string, error) {
	switch name {
	case "ip":
		return e.GetIP(), nil
	case "netns":
		return fmt.Sprintf("ns%d", e.GetNetns()), nil
	default:
		return "", fmt.Errorf("unkonw or unsupported meta %s", name)
	}
}

func (e *Entity) IsHostNetwork() bool {
	return e.netnsMeta.isHostNetwork
}

func (e *Entity) GetNetns() int {
	return e.netnsMeta.inum
}

func (e *Entity) GetNetnsMountPoint() string {
	return e.netnsMeta.mountPath
}

func (e *Entity) GetPodSandboxID() string {
	return e.podMeta.sandbox
}

func (e *Entity) GetNsHandle() (netns.NsHandle, error) {
	if len(e.pids) != 0 {
		return netns.GetFromPid(e.pids[0])
	}

	return netns.GetFromPath(e.netnsMeta.mountPath)
}

func (e *Entity) GetNetNsFd() (int, error) {
	h, err := e.GetNsHandle()
	if err != nil {
		return InitNetns, err
	}

	return int(h), nil
}

// GetPid return a random pid of entify, if no process in netns,return 0
func (e *Entity) GetPid() int {
	if len(e.pids) == 0 {
		return 0
	}
	return e.pids[0]
}
func (e *Entity) GetPids() []int {
	return e.pids
}

func StartCache(ctx context.Context) error {
	slog.Ctx(ctx).Info("nettop cache loop statrt", "interval", cacheUpdateInterval)
	return cacheDaemonLoop(ctx, control)
}

func StopCache() {
	control <- struct{}{}
}

func cacheDaemonLoop(ctx context.Context, control chan struct{}) error {
	t := time.NewTicker(cacheUpdateInterval)
	defer t.Stop()

	for {
		select {
		case <-control:
			slog.Ctx(ctx).Info("cache daemon loop exit of control signal")
			return nil
		case <-t.C:
			go cacheProcess()
		}

	}

}

func cacheProcess() {
	start := time.Now()
	ctx, cancelf := context.WithTimeout(context.Background(), cacheUpdateInterval)
	defer cancelf()

	cacheDone := make(chan struct{})
	go func(done chan struct{}) {
		err := cacheNetTopology()
		if err != nil {
			logger.Warn("cache process", "err", err)
		}
		done <- struct{}{}
	}(cacheDone)

	select {
	case <-ctx.Done():
		logger.Info("cache process time exceeded", "latency", time.Since(start).Seconds())
		return
	case <-cacheDone:
		logger.Info("cache process finished", "latency", time.Since(start).Seconds())
	}

}

func SyncNetTopology() error {
	return cacheNetTopology()
}

func cacheNetTopology() error {
	// get all process
	pids, err := getAllPids()
	if err != nil {
		logger.Warn("cache pids failed %s", err.Error())
		return err
	}

	logger.Debug("finished get all pids")
	// get all netns by process
	netnsMap := map[int]netnsMeta{}
	for _, pid := range pids {
		nsinum, err := getNsInumByPid(pid)
		if err != nil {
			logger.Warn("get ns inum of %d failed %s", pid, err.Error())
			continue
		}

		if v, ok := netnsMap[nsinum]; !ok {
			nsm := netnsMeta{
				inum: nsinum,
				pids: []int{pid},
			}
			if pid == 1 {
				nsm.isHostNetwork = true
			}
			netnsMap[nsinum] = nsm
		} else {
			v.pids = append(v.pids, pid)
			if pid == 1 {
				v.isHostNetwork = true
			}
			netnsMap[nsinum] = v
		}

	}

	logger.Debug("finished get all netns")

	// get netns mount point aka cni presentation
	namedns, err := findNsfsMountpoint()
	if err != nil {
		logger.Warn("get nsfs mount point failed %s", err.Error())
	} else {
		for _, mp := range namedns {
			nsinum, err := getNsInumByNsfsMountPoint(mp)
			if err != nil {
				logger.Warn("get ns inum from %s point failed %s", mp, err.Error())
				continue
			}
			if v, ok := netnsMap[nsinum]; !ok {
				// in rund case, netns does not have any live process
				netnsMap[nsinum] = netnsMeta{
					inum:      nsinum,
					mountPath: mp,
				}
			} else {
				v.mountPath = mp
				netnsMap[nsinum] = v
			}
		}
	}

	logger.Debug("finished get all nsfs mount point")

	// get pod meta info
	podMap, err := getPodMetas(rcrisvc)
	if err != nil {
		logger.Warn("get pod meta failed %s", err.Error())
		return err
	}

	// if use docker, get docker sandbox
	if top.Crimeta.RuntimeName == "docker" {
		for sandbox, pm := range podMap {
			if pm.nspath == "" && pm.pid == 0 {
				pid, err := getPidForContainerBySock(sandbox)
				if err != nil {
					logger.Warn("get docker container", "sandbox", sandbox, "err", err.Error())
					continue
				}
				pm.pid = pid
			}
			podMap[sandbox] = pm
		}
	}

	// combine netns and pod cache,
	for nsinum, nsmeta := range netnsMap {
		ent := &Entity{
			netnsMeta: nsmeta,
			pids:      nsmeta.pids,
		}
		logger.Debug("try related  pod", nsinum, nsmeta.mountPath)
		for sandbox, pm := range podMap {
			// 1. use cri infospec/nspath to match
			if pm.nspath != "" &&
				(pm.nspath == nsmeta.mountPath || pm.nspath == fmt.Sprintf("/var/%s", nsmeta.mountPath)) {
				ent.podMeta = pm
				logger.Debug("related pod mount point", "pod", pm.name, "netns", nsmeta.inum)
				podCache.Set(sandbox, ent, 3*cacheUpdateInterval)
				for _, pid := range nsmeta.pids {
					pidCache.Set(fmt.Sprintf("%d", pid), ent, 3*cacheUpdateInterval)
				}
				continue
			}

			// 2. use pid nsinum to match
			pidns, err := getNsInumByPid(pm.pid)
			if err == nil {
				if nsinum == pidns {
					ent.podMeta = pm
					logger.Debug("related pod", "pod", pm.name, "netns", nsmeta.inum)
					podCache.Set(sandbox, ent, 3*cacheUpdateInterval)
					for _, pid := range nsmeta.pids {
						pidCache.Set(fmt.Sprintf("%d", pid), ent, 3*cacheUpdateInterval)
					}
				}
			} else {
				// 3. try to use pid to match
				for _, pid := range nsmeta.pids {
					if pm.pid == pid {
						ent.podMeta = pm
						logger.Debug("related pod pid", "pod", pm.name, "netns", nsmeta.inum)
						podCache.Set(sandbox, ent, 3*cacheUpdateInterval)
						for _, pid := range nsmeta.pids {
							pidCache.Set(fmt.Sprintf("%d", pid), ent, 3*cacheUpdateInterval)
						}
					}
				}
			}
		}
		nsCache.Set(fmt.Sprintf("%d", nsinum), ent, 3*cacheUpdateInterval)
	}

	logger.Debug("finished cache process")
	return nil
}
