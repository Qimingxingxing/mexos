package shardmaster

import "net"
import "fmt"
import "net/rpc"
import "log"
import "paxos"
import "sync"
import "os"
import "syscall"
import "encoding/gob"
import "math/rand"
import "time"

//import "math"
import "strconv"

const (
	Join   = "Join"
	Leave  = "Leave"
	Move   = "Move"
	Query  = "Query"
	Noop   = "Noop"
	Add    = "Add"
	Remove = "Remove"
)

type ShardMaster struct {
	mu         sync.Mutex
	l          net.Listener
	me         int
	dead       bool // for testing
	unreliable bool // for testing
	px         *paxos.Paxos

	configs        []Config         // indexed by config num
	highestApplied int              //the highest paxos operation applied to our loggy log
	outstanding    map[int]chan *Op //requests we have yet to respond to
	count          int
}

type Op struct {
	Type      string
	ID        string
	JoinArgs  *JoinArgs
	LeaveArgs *LeaveArgs
	MoveArgs  *MoveArgs
	QueryArgs *QueryArgs
}

//clear out any previous data in case it was a recursive call
func (sm *ShardMaster) clearReply(reply interface{}) {
	r, isQueryReply := reply.(QueryReply)
	if isQueryReply {
		r.Config = Config{}
	}
}

//TODO: don't change configs if move to self or duplicate join/leave

func (sm *ShardMaster) Join(args *JoinArgs, reply *JoinReply) error {
	/*
		sm.mu.Lock()
		cur := sm.configs[len(sm.configs)-1]
		//quit without doing anything if group exists
		if _,ok := cur.Groups[args.GID]; ok {
			sm.mu.Unlock()
			return nil
		}
		sm.mu.Unlock()
	*/
	sm.clearReply(reply)
	seq, c, ID := sm.seqChanID()
	proposedVal := Op{Type: Join, ID: ID, JoinArgs: args}

	sm.px.Start(seq, proposedVal)
	acceptedVal := <-c
	//start over if it wasn't what we proposed
	if proposedVal.ID != acceptedVal.ID { //is this comparison sufficient?
		sm.mu.Lock()
		delete(sm.outstanding, seq)
		c <- &Op{}
		sm.mu.Unlock()
		sm.Join(args, reply)
		return nil
	}

	//housecleaning
	sm.mu.Lock()
	delete(sm.outstanding, seq)
	c <- &Op{}
	sm.mu.Unlock()
	return nil
}

func (sm *ShardMaster) Leave(args *LeaveArgs, reply *LeaveReply) error {
	/*sm.mu.Lock()
	cur := sm.configs[len(sm.configs)-1]
	//quit without doing anything if group is gone
	if _,ok := cur.Groups[args.GID]; !ok {
		sm.mu.Unlock()
		return nil
	}
	sm.mu.Unlock()*/

	seq, c, ID := sm.seqChanID()

	proposedVal := Op{Type: Leave, ID: ID, LeaveArgs: args}

	sm.px.Start(seq, proposedVal)
	acceptedVal := <-c

	//start over if it wasn't what we proposed
	if proposedVal.ID != acceptedVal.ID { //is this comparison sufficient?
		sm.mu.Lock()
		delete(sm.outstanding, seq)
		c <- &Op{}
		sm.mu.Unlock()
		sm.Leave(args, reply)
		return nil
	}

	//housecleaning
	sm.mu.Lock()
	delete(sm.outstanding, seq)
	c <- &Op{}
	sm.mu.Unlock()
	return nil
}

func (sm *ShardMaster) Move(args *MoveArgs, reply *MoveReply) error {
	/*sm.mu.Lock()
	cur := sm.configs[len(sm.configs)-1]
	//quit without doing anything if shard already assigned to group
	if cur.Shards[args.Shard] == args.GID {
		sm.mu.Unlock()
		return nil
	}
	sm.mu.Unlock()*/

	seq, c, ID := sm.seqChanID()

	proposedVal := Op{Type: Move, ID: ID, MoveArgs: args}

	sm.px.Start(seq, proposedVal)
	acceptedVal := <-c

	//start over if it wasn't what we proposed
	if proposedVal.ID != acceptedVal.ID { //is this comparison sufficient?
		sm.mu.Lock()
		delete(sm.outstanding, seq)
		c <- &Op{}
		sm.mu.Unlock()
		sm.Move(args, reply)
		return nil
	}

	//housecleaning
	sm.mu.Lock()
	delete(sm.outstanding, seq)
	c <- &Op{}
	sm.mu.Unlock()
	return nil
}

func (sm *ShardMaster) Query(args *QueryArgs, reply *QueryReply) error {
	seq, c, ID := sm.seqChanID()
	sm.clearReply(reply)
	proposedVal := Op{Type: Query, ID: ID, QueryArgs: args}
	sm.px.Start(seq, proposedVal)
	acceptedVal := <-c
	//start over if it wasn't what we proposed
	if proposedVal.ID != acceptedVal.ID { //is this comparison sufficient?
		sm.mu.Lock()
		delete(sm.outstanding, seq)
		c <- &Op{}
		sm.mu.Unlock()
		sm.Query(args, reply)
		return nil
	}

	sm.mu.Lock()
	if args.Num != -1 && args.Num < len(sm.configs) {
		reply.Config = sm.configs[args.Num]
	} else { //else, get latest
		reply.Config = sm.configs[len(sm.configs)-1]

	}
	sm.mu.Unlock()

	//housecleaning
	sm.mu.Lock()
	delete(sm.outstanding, seq)
	c <- &Op{}
	sm.mu.Unlock()
	return nil
}

func (sm *ShardMaster) seqChanID() (int, chan *Op, string) {
	sm.mu.Lock()
	seq := sm.px.Max() + 1
	for _, ok := sm.outstanding[seq]; ok || sm.highestApplied >= seq; {
		seq++
		_, ok = sm.outstanding[seq]
	}
	ID := strconv.Itoa(sm.me) + "-" + strconv.Itoa(sm.count)
	sm.count++
	c := make(chan *Op)
	sm.outstanding[seq] = c
	sm.mu.Unlock()
	return seq, c, ID
}

func (sm *ShardMaster) getStatus() {
	seq := 0
	restarted := false
	//TA suggests 10-20ms max before proposing noop
	to := 10 * time.Millisecond
	for !sm.dead {
		decided, r := sm.px.Status(seq)
		if decided {

			//add to log and notify channels
			op, isOp := r.(Op)
			if isOp {
				sm.mu.Lock()
				sm.apply(seq, &op)
				if ch, ok := sm.outstanding[seq]; ok {
					ch <- &op //notify handler
					sm.mu.Unlock()
					<-ch
				} else {
					sm.mu.Unlock()
				}
			} else {
				log.Fatal("Fatal!could not cast Op")
			}
			seq++
			restarted = false
		} else {
			time.Sleep(to)
			if to < 25*time.Millisecond {
				to *= 2
			} else { //if we've slept long enough, propose a noop
				if !restarted {
					to = 10 * time.Millisecond
					sm.px.Start(seq, Op{Type: Noop})
					time.Sleep(time.Millisecond)
					restarted = true
				}
			}
		}
	}
}

func (sm *ShardMaster) apply(seq int, op *Op) {
	old := sm.configs[len(sm.configs)-1]
	c := Config{}
	switch op.Type {
	//Join: Create a new configuration that includes the new replica
	//group. Divide the shards as evenly as possible among the
	//groups, move as few shards as possible to achieve that goal
	case Join:
		c.Num = len(sm.configs)
		c.Shards = sm.redistribute(Add, op.JoinArgs.GID)
		c.Groups = sm.modMap(Add, op.JoinArgs.GID, op.JoinArgs.Servers)
		sm.configs = append(sm.configs, c)

	//Leave: Create a new configuration that does not include the group,
	//and that assigns the group's shards to the remaining groups.
	case Leave:
		c.Num = len(sm.configs)
		c.Shards = sm.redistribute(Remove, op.LeaveArgs.GID)
		c.Groups = sm.modMap(Remove, op.LeaveArgs.GID, make([]string, 1))
		sm.configs = append(sm.configs, c)
	//The shardmaster should create a new configuration in which
	//the shard is assigned to the group. The main purpose of Move
	//is to allow us to test your software
	case Move:
		n := Config{}
		n.Num = len(sm.configs)
		n.Shards = old.Shards
		n.Shards[op.MoveArgs.Shard] = op.MoveArgs.GID
		n.Groups = make(map[int64][]string)
		for k, v := range old.Groups {
			n.Groups[k] = v
		}
		sm.configs = append(sm.configs, n)

	//The shardmaster replies with the configuration that has that
	//number. If the number is -1 or bigger than the biggest known
	//configuration number, the shardmaster should reply with the
	//latest configuration.
	case Query:
		//do nothing
	case Noop:
		//do nothing
	}

	sm.highestApplied = seq
	sm.px.Done(seq)
}

func (sm *ShardMaster) redistribute(op string, GID int64) [10]int64 {
	current := sm.configs[len(sm.configs)-1]
	/*var groups = map[int64]bool {
		1:true,
		2:true,
		3:true,
		4:true,
		5:true,
		6:true,
		7:true,
		8:true,
		9:true,
		10:true,
		11:true,
	}*/
	count := make(map[int64]int)
	for k, _ := range current.Groups {
		count[k] = 0
	}
	//old  := [10]int64{1,2,3,4,5,6,7,8,9,10}
	newShards := current.Shards

	switch op {
	case Add:
		count[GID] = 0
		for i, v := range newShards {
			if v == 0 {
				newShards[i] = GID
				count[GID] += 1
			} else {
				count[v] += 1
			}
		}
	case Remove:
		delete(count, GID)
		var random int64
		//find a random group to xfer ownership to temporarily
		for _, v := range newShards {
			if v != GID {
				random = v
				break
			}
		}
		for i, v := range newShards {
			if v == GID {
				newShards[i] = random
			}
		}
		for _, v := range newShards {
			count[v] += 1
		}
	}
	return rebalance(count, newShards)
}

func rebalance(count map[int64]int, newShards [10]int64) [10]int64 {
	//Moves a shard from the group with the most shards to the
	//group with the least shards until the group with the most
	//shards has at most one shard more than the group with the
	//least shards.
	least := getLeast(count)
	most := getMost(count)
	for count[least]+1 < count[most] {
		newShards = move(most, least, newShards)
		count[most] -= 1
		count[least] += 1
		least = getLeast(count)
		most = getMost(count)
	}
	return newShards
}

func move(from int64, to int64, a [10]int64) [10]int64 {
	fi := -1
	for i := 0; i < len(a); i++ {
		if a[i] == from {
			fi = i
		}
	}
	//do the moving
	a[fi] = to
	return a
}

func getLeast(count map[int64]int) int64 {
	var lowestKey int64
	lowestValue := NShards + 1
	for k, v := range count {
		if v < lowestValue {
			lowestValue = v
			lowestKey = k
		}
	}
	return lowestKey
}

func getMost(count map[int64]int) int64 {

	var highestKey int64
	highestValue := 0
	for k, v := range count {
		if v >= highestValue {
			highestValue = v
			highestKey = k
		}
	}
	return highestKey
}

func (sm *ShardMaster) modMap(op string, GID int64,
	servers []string) map[int64][]string {

	current := sm.configs[len(sm.configs)-1]
	newMap := make(map[int64][]string)
	for k, v := range current.Groups {
		newMap[k] = v
	}

	switch op {
	case Add:
		newMap[GID] = servers
	case Remove:
		delete(newMap, GID)
	}
	return newMap
}

// please don't change this function.
func (sm *ShardMaster) Kill() {
	sm.dead = true
	sm.l.Close()
	sm.px.Kill()
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant shardmaster service.
// me is the index of the current server in servers[].
//
func StartServer(servers []string, me int) *ShardMaster {
	gob.Register(Op{})
	gob.Register(JoinArgs{})
	gob.Register(LeaveArgs{})
	gob.Register(MoveArgs{})
	gob.Register(QueryArgs{})

	sm := new(ShardMaster)
	sm.me = me

	sm.configs = make([]Config, 1)
	sm.configs[0].Groups = map[int64][]string{}
	//sm.configs[0].Shards = make([]int64, NShards)
	sm.outstanding = make(map[int]chan *Op)
	sm.highestApplied = -1
	sm.count = 0

	rpcs := rpc.NewServer()
	rpcs.Register(sm)

	sm.px = paxos.Make(servers, me, rpcs)

	os.Remove(servers[me])
	l, e := net.Listen("unix", servers[me])
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	sm.l = l

	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for sm.dead == false {
			conn, err := sm.l.Accept()
			if err == nil && sm.dead == false {
				if sm.unreliable && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if sm.unreliable && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					c1 := conn.(*net.UnixConn)
					f, _ := c1.File()
					err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
					if err != nil {
						fmt.Printf("shutdown: %v\n", err)
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && sm.dead == false {
				fmt.Printf("ShardMaster(%v) accept: %v\n", me, err.Error())
				sm.Kill()
			}
		}
	}()

	go sm.getStatus()
	return sm
}
