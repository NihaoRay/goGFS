package master

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"net"
	"net/rpc"
	"time"

	"gfs"
	"gfs/util"
)

// Master Server struct
type Master struct {
	address    gfs.ServerAddress // master server address
	serverRoot string
	l          net.Listener
	shutdown   chan struct{}

	nm  *namespaceManager
	cm  *chunkManager
	csm *chunkServerManager
}

// NewAndServe starts a master and returns the pointer to it.
func NewAndServe(address gfs.ServerAddress, serverRoot string) *Master {
	m := &Master{
		address:    address,
		serverRoot: serverRoot,
		shutdown:   make(chan struct{}),
	}

	rpcs := rpc.NewServer()
	rpcs.Register(m)
	l, e := net.Listen("tcp", string(m.address))
	if e != nil {
		log.Fatal("listen error:", e)
		log.Exit(1)
	}
	m.l = l

	m.initMetadata()

	// RPC Handler
	go func() {
	loop:
		for {
			select {
			case <-m.shutdown:
				break loop
			default:
			}
			conn, err := m.l.Accept()
			if err == nil {
				go func() {
					rpcs.ServeConn(conn)
					conn.Close()
				}()
			} else {
				log.Fatal("accept error:", err)
				log.Exit(1)
			}
		}
	}()

	// Background Task
	go func() {
		ticker := time.Tick(gfs.BackgroundInterval)
		for {
			<-ticker

			err := m.backgroundActivity()
			if err != nil {
				log.Fatal("Background error ", err)
			}
		}
	}()

	log.Infof("Master is running now. addr = %v", address)

	return m
}

// InitMetadata initiates meta data
func (m *Master) initMetadata() {
	// new or read from old
	m.nm = newNamespaceManager()
	m.cm = newChunkManager()
	m.csm = newChunkServerManager()
	return
}

// Shutdown shuts down master
func (m *Master) Shutdown() {
	close(m.shutdown)
	//m.l.Close()
}

// BackgroundActivity does all the background activities
// server disconnection handle, garbage collection, stale replica detection, etc
func (m *Master) backgroundActivity() error {
	// detect dead servers
	addrs := m.csm.DetectDeadServers()
	for _, v := range addrs {
		log.Warningf("remove server %v", v)
		handles, err := m.csm.RemoveServer(v)
		if err != nil {
			return err
		}
		m.cm.RemoveChunks(handles, v)
	}

	// add replicas for need request
	handles := m.cm.GetNeedlist()
	log.Info("Master Background ", handles)
	if handles != nil {
		for i := 0; i < len(handles); i++ {
			ck := m.cm.chunk[handles[i]]

			if ck.expire.Before(time.Now()) {
				ck.Lock() // don't grant lease during copy
				err := m.reReplication(handles[i])
				log.Info(err)
				ck.Unlock()
			}
		}
	}
	return nil
}

// perform re-Replication
func (m *Master) reReplication(handle gfs.ChunkHandle) error {
	// lock chunk, so master will not grant lease in copy time
	from, to, err := m.csm.ChooseReReplication(handle)
	if err != nil {
		return err
	}
	log.Warningf("allocate new chunk %v from %v to %v", handle, from, to)

	var cr gfs.CreateChunkReply
	err = util.Call(to, "ChunkServer.RPCCreateChunk", gfs.CreateChunkArg{handle}, &cr)
	if err != nil {
		return err
	}

	var sr gfs.SendCopyReply
	err = util.Call(from, "ChunkServer.RPCSendCopy", gfs.SendCopyArg{handle, to}, &sr)
	if err != nil {
		return err
	}

	m.cm.RegisterReplica(handle, to)
	m.csm.AddChunk([]gfs.ServerAddress{to}, handle)
	return nil
}


func (m *Master) loadMeta() error {
    return nil
}

func (m *Master) storeMeta() error {
    return nil
}

// RPCHeartbeat is called by chunkserver to let the master know that a chunkserver is alive
func (m *Master) RPCHeartbeat(args gfs.HeartbeatArg, reply *gfs.HeartbeatReply) error {
	rep := m.csm.Heartbeat(args.Address)
	if rep != nil { // load reported chunks
		for _, v := range rep {
			m.cm.RegisterReplica(v.Handle, args.Address)
			m.csm.AddChunk([]gfs.ServerAddress{args.Address}, v.Handle)
		}
	}

	for _, handle := range args.LeaseExtensions {
		m.cm.ExtendLease(handle, args.Address)
	}
	return nil
}

// RPCGetPrimaryAndSecondaries returns lease holder and secondaries of a chunk.
// If no one holds the lease currently, grant one.
func (m *Master) RPCGetPrimaryAndSecondaries(args gfs.GetPrimaryAndSecondariesArg, reply *gfs.GetPrimaryAndSecondariesReply) error {
	lease, err := m.cm.GetLeaseHolder(args.Handle)
	if err != nil {
		return err
	}
	reply.Primary = lease.Primary
	reply.Expire = lease.Expire
	reply.Secondaries = lease.Secondaries
	return nil
}

// RPCExtendLease extends the lease of chunk if the lessee is nobody or requester.
func (m *Master) RPCExtendLease(args gfs.ExtendLeaseArg, reply *gfs.ExtendLeaseReply) error {
	//t, err := m.cm.ExtendLease(args.Handle, args.Address)
	//if err != nil { return err }
	//reply.Expire = *t
	return nil
}

// RPCGetReplicas is called by client to find all chunkserver that holds the chunk.
func (m *Master) RPCGetReplicas(args gfs.GetReplicasArg, reply *gfs.GetReplicasReply) error {
	servers, err := m.cm.GetReplicas(args.Handle)
	if err != nil {
		return err
	}

	for _, v := range servers.GetAll() {
		reply.Locations = append(reply.Locations, v.(gfs.ServerAddress))
	}

	return nil
}

// RPCCreateFile is called by client to create a new file
func (m *Master) RPCCreateFile(args gfs.CreateFileArg, replay *gfs.CreateFileReply) error {
	err := m.nm.Create(args.Path)
	return err
}

// RPCDelete is called by client to delete a file
func (m *Master) RPCDelete(args gfs.DeleteFileArg, replay *gfs.DeleteFileReply) error {
	log.Fatal("call to unimplemented RPCDelete")
	return nil
}

// RPCMkdir is called by client to make a new directory
func (m *Master) RPCMkdir(args gfs.MkdirArg, replay *gfs.MkdirReply) error {
	err := m.nm.Mkdir(args.Path)
	return err
}

// RPCList is called by client to list all files in specific directory
func (m *Master) RPCList(args gfs.ListArg, replay *gfs.ListReply) error {
	log.Fatal("call to unimplemented RPCList")
	return nil
}

// RPCGetFileInfo is called by client to get file information
func (m *Master) RPCGetFileInfo(args gfs.GetFileInfoArg, reply *gfs.GetFileInfoReply) error {
	ps, cwd, err := m.nm.lockParents(args.Path)
	defer m.nm.unlockParents(ps)
	if err != nil {
		return err
	}

	file, ok := cwd.children[ps[len(ps)-1]]
	if !ok {
		return fmt.Errorf("File %v does not exist", args.Path)
	}
	file.RLock()
	defer file.RUnlock()

	reply.IsDir = file.isDir
	reply.Length = file.length
	reply.Chunks = file.chunks
	return nil
}

// RPCGetChunkHandle returns the chunk handle of (path, index).
// If the requested index is bigger than the number of chunks of this path by one, create one.
func (m *Master) RPCGetChunkHandle(args gfs.GetChunkHandleArg, reply *gfs.GetChunkHandleReply) error {
	ps, cwd, err := m.nm.lockParents(args.Path)
	defer m.nm.unlockParents(ps)
	if err != nil {
		return err
	}

	// append new chunks
	file, ok := cwd.children[ps[len(ps)-1]]
	if !ok {
		return fmt.Errorf("File %v does not exist", args.Path)
	}
	file.Lock()
	defer file.Unlock()

	if int(args.Index) == int(file.chunks) {
		file.chunks++

		addrs, err := m.csm.ChooseServers(gfs.DefaultNumReplicas)
		if err != nil {
			return err
		}

		reply.Handle, addrs, err = m.cm.CreateChunk(args.Path, addrs)
		if err != nil {
			// WARNING
			log.Warning("[ignored] An ignored error in RPCGetChunkHandle ", err)
			return nil
		}

		m.csm.AddChunk(addrs, reply.Handle)
	} else {
		reply.Handle, err = m.cm.GetChunk(args.Path, args.Index)
	}

	return err
}
