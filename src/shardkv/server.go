package shardkv

import "net"
import "fmt"
import "net/rpc"
import "log"
import "time"
import "paxos"
import "sync"
import "os"
import "errors"

import "io"
import "bufio"
import "encoding/binary"
import "syscall"
import "encoding/gob"
import "math/rand"
import "shardmaster"
import "strconv"

import "github.com/jmhodges/levigo"
import "bytes"
import "strings"

import "runtime"
import "os/exec"

const Debug = 0
const DebugPersist = 0
const printRPCerrors = false
const Log = 0

var logfile *os.File

// DATABASE / MEMORY CONFIGURATION
// Remember to set the corresponding variables in shardmaster and paxos
// for accurate testing!
const persistent = true
const recovery = true
const dbUseCompression = true                  // Whether database should compress entries
const dbUseCache = true                        // Whether database should use a built-in cache
const dbCacheSize = 20                         // Size of database cache in MB (ignored if dbUseCache is false)
const memoryLimit = 100                        // Memory limit in MB
const memoryThreshold = memoryLimit * 75 / 100 // When to stop filling memory (when to abort a Fetch RPC and use multiple messages)
const recoveryRetryDelay = 500                 // Time in ms to wait before resending acknowledgments
const recoveryPortNum = 3222

// Will use these to check that dbCacheSize doesn't overflow an int
// (int size is either 32 or 64 bits depending on implementation)
const MaxUint = ^uint(0)
const MaxInt = int(^uint(0) >> 1)

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

func DPrintfPersist(format string, a ...interface{}) (n int, err error) {
	if DebugPersist > 0 {
		fmt.Printf(format, a...)
	}
	return
}

type Op struct {
	Op       int //1 = Get, 2 = Put, 3 = PutHash, 4 = Reconfigure
	OpID     int64
	ClientID int64
	Key      string
	Value    string

	ConfigNum int
	Store     map[string]string // key/value store
	Response  map[int64]string  // client responses, indexed by client ID
	Seen      map[int64]bool    // which ops have been seen, indexed by op ID
}

type ShardKV struct {
	mu        sync.Mutex
	l         net.Listener
	dead      bool // for testing
	dbClosed  bool
	dbDeleted bool

	// Network stuff
	me         int
	unreliable bool // for testing
	network    bool
	servers    []string

	// ShardKV state
	sm       *shardmaster.Clerk
	px       *paxos.Paxos
	gid      int64 // my replica group ID
	config   shardmaster.Config
//	store    map[string]string // key/value store
//	response map[int64]string  // client responses, indexed by client ID
//	seen     map[int64]bool    // which ops have been seen, indexed by op ID
	minSeq   int

	// Persistence stuff
	dbReadOptions  *levigo.ReadOptions
	dbWriteOptions *levigo.WriteOptions
	dbOpts         *levigo.Options
	dbName         string
	db             *levigo.DB
	dbLock         sync.Mutex
	recovering     bool
	sending        bool
	sendingTo      string
}

// Write the desired key/value to memory and/or disk
func (kv *ShardKV) putValue(key string, value string) {
	// Write to disk if persistent is enabled
	kv.dbPut(key, value)
}

// Get the desired value, either from memory or disk
func (kv *ShardKV) getValue(key string) (string, bool) {
	value, exists := kv.dbGet(key)
	return value, exists
}

// Write the seen opID to memory and/or disk
func (kv *ShardKV) putSeen(opID int64, seen bool) {
	// Write to memory if using memory
	// if writeToMemory {
	// 	kv.seen[opID] = seen
	// }
	// Write to disk if persistent is enabled
	kv.dbWriteSeen(opID, seen)
}

// Get whether the op is seen, either from memory or disk
func (kv *ShardKV) getSeen(opID int64) bool {
	seen := kv.dbGetSeen(opID)
	return seen
}

// Write the desired response to memory and/or disk
func (kv *ShardKV) putResponse(opID int64, clientID int64, value string) {
	// Write to disk if persistent is enabled
	kv.dbWriteResponse(opID, clientID, value)
}

// Get the desired response, either from memory or disk
func (kv *ShardKV) getResponse(opID int64, clientID int64) (string, bool) {
	response, exists := kv.dbGetResponse(opID, clientID)
	return response, exists
}

// Process log entries up until the given sequence
func (kv *ShardKV) processLog(maxSeq int) {
	if maxSeq <= kv.minSeq+1 {
		return
	}
	DPrintf("%d.%d.%d) Process Log Until %d\n", kv.gid, kv.me, kv.config.Num, maxSeq)

	for i := kv.minSeq + 1; i < maxSeq; i++ {
		to := 10 * time.Millisecond
		start := false
		// Get decided value or propose a no-op
		for !kv.dead {
			decided, opp := kv.px.Status(i)
			if decided {
				op := opp.(Op)
				if op.Op == 1 {
					DPrintf("%d.%d.%d) Log %d: Op #%d - GET(%s)\n", kv.gid, kv.me, kv.config.Num, i, op.OpID, op.Key)
					// Write the response to memory and disk
					val, _ := kv.getValue(op.Key)
					kv.putResponse(op.OpID, op.ClientID, val)
				} else if op.Op == 2 {
					DPrintf("%d.%d.%d) Log %d: Op #%d - PUT(%s, %s)\n", kv.gid, kv.me, kv.config.Num, i, op.OpID, op.Key, op.Value)
					// Write the response to memory and disk
					val, _ := kv.getValue(op.Key)
					kv.putResponse(op.OpID, op.ClientID, val)
					// Write the value to memory and/or disk
					kv.putValue(op.Key, op.Value)
				} else if op.Op == 3 {
					DPrintf("%d.%d.%d) Log %d: Op #%d - PUTHASH(%s, %s)\n", kv.gid, kv.me, kv.config.Num, i, op.OpID, op.Key, op.Value)
					// Write the response to memory and disk
					val, _ := kv.getValue(op.Key)
					kv.putResponse(op.OpID, op.ClientID, val)
					// Write the value to memory and disk
					val = strconv.Itoa(int(hash(val + op.Value)))
					kv.putValue(op.Key, val)
				} else if op.Op == 4 {
					DPrintf("%d.%d.%d) Log %d: Op #%d - RECONFIGURE(%d)\n", kv.gid, kv.me, kv.config.Num, i, op.OpID, op.ConfigNum)
					// Write the new shard data to memory and disk
					for nk, nv := range op.Store {
						kv.putValue(nk, nv)
					}
					// Write the new responses to memory and disk
					for clientID, value := range op.Response {
						kv.putResponse(-1, clientID, value)
					}
					// Write seen op IDs to memory and disk
					for opID, _ := range op.Seen {
						kv.putSeen(opID, true)
					}
					// Record the new config in memory and disk
					kv.config = kv.sm.Query(op.ConfigNum)
					kv.dbWriteConfigNum(kv.config.Num)
				}
				break
			} else if !start {
				kv.px.Start(i, Op{})
				start = true
			}
			time.Sleep(to)
			if to < 1*time.Second {
				to *= 2
			}
		}
	}
	// Update the new minSeq in memory and disk
	kv.minSeq = maxSeq - 1
	kv.dbWriteMinSeq(kv.minSeq)
	kv.px.Done(kv.minSeq)
}

// Log the given op and execute it
func (kv *ShardKV) processKV(op Op, reply *KVReply) {
	for !kv.dead {
		// Process any missed log entries
		seq := kv.px.Max() + 1
		kv.processLog(seq)
		// If wrong group for shard, return
		if kv.config.Shards[key2shard(op.Key)] != kv.gid {
			return
		}
		// If duplicate request, use previous response
		if v, seen := kv.getResponse(op.OpID, op.ClientID); seen {
			DPrintf("%d.%d.%d) Already Seen Op %d\n", kv.gid, kv.me, kv.config.Num, op.OpID)
			if v == "" {
				reply.Err = ErrNoKey
			} else {
				reply.Err = OK
			}
			reply.Value = v
			return
		}

		// Propose desired op to Paxos log
		kv.px.Start(seq, op)
		to := 10 * time.Millisecond
		for !kv.dead {
			// Check if sequence has been decided
			if decided, _ := kv.px.Status(seq); decided {
				// Process any missed log entries
				seq := kv.px.Max() + 1
				kv.processLog(seq)
				// If wrong group for shard, return
				if kv.config.Shards[key2shard(op.Key)] != kv.gid {
					return
				}
				// If have seen op (duplicate or just decided), return response
				if v, seen := kv.getResponse(op.OpID, op.ClientID); seen {
					if v == "" {
						reply.Err = ErrNoKey
					} else {
						reply.Err = OK
					}
					reply.Value = v
					return
				} else {
					break
				}
			}

			time.Sleep(to)
			if to < 1*time.Second {
				to *= 2
			}
		}
	}
}

// Log and execute a reconfiguration
func (kv *ShardKV) addReconfigure(num int, store map[string]string, response map[int64]string, seen map[int64]bool) {
	defer func() {
		DPrintf("%d.%d.%d) Reconfigure Returns\n", kv.gid, kv.me, kv.config.Num)
	}()

	newOp := Op{}
	newOp.Op = 4
	newOp.OpID = int64(num)
	newOp.ClientID = -1
	newOp.ConfigNum = num
	newOp.Store = store
	newOp.Response = response
	newOp.Seen = seen
	DPrintf("%d.%d.%d) Reconfigure: %d\n", kv.gid, kv.me, kv.config.Num, num)

	for !kv.dead {
		// Process any missed log entries
		seq := kv.px.Max() + 1
		kv.processLog(seq)
		// If desired config is now out of date, return
		if kv.config.Num >= num {
			return
		}

		// Propose reconfiguration to Paxos
		kv.px.Start(seq, newOp)

		to := 10 * time.Millisecond
		for !kv.dead {
			// Check if sequence has been decided
			if decided, _ := kv.px.Status(seq); decided {
				// Process any missed log entries
				seq := kv.px.Max() + 1
				kv.processLog(seq)
				// If config is updated, return
				if kv.config.Num >= num {
					return
				} else {
					break
				}
			}

			time.Sleep(to)
			if to < 1*time.Second {
				to *= 2
			}
		}
	}
}

// Accept a Get request
func (kv *ShardKV) Get(args *GetArgs, reply *KVReply) error {
	for (kv.recovering || kv.sending) && !kv.dead {
		time.Sleep(10 * time.Millisecond)
	}
	kv.mu.Lock()
	defer func() {
		DPrintf("%d.%d.%d) Get Returns: %s (%s)\n", kv.gid, kv.me, kv.config.Num, reply.Value, reply.Err)
		kv.mu.Unlock()
	}()

	reply.Err = ErrWrongGroup

	newOp := Op{}
	newOp.Op = 1
	newOp.OpID = args.ID
	newOp.ClientID = args.ClientID
	newOp.Key = args.Key
	DPrintf("%d.%d.%d) Get: %s\n", kv.gid, kv.me, kv.config.Num, args.Key)

	kv.processKV(newOp, reply)
	return nil
}

// Accept a Put request
func (kv *ShardKV) Put(args *PutArgs, reply *KVReply) error {
	if args.DoHash {
		DPrintf("%d.%d.%d) PutHash: %s -> %s\n", kv.gid, kv.me, kv.config.Num, args.Key, args.Value)
	} else {
		DPrintf("%d.%d.%d) Put: %s -> %s\n", kv.gid, kv.me, kv.config.Num, args.Key, args.Value)
	}
	for (kv.recovering || kv.sending) && !kv.dead {
		time.Sleep(10 * time.Millisecond)
	}
	kv.mu.Lock()
	defer func() {
		DPrintf("%d.%d.%d) Put Returns: %s (%s)\n", kv.gid, kv.me, kv.config.Num, reply.Value, reply.Err)
		if reply.Err == ErrNoKey {
			reply.Err = OK
		}
		kv.mu.Unlock()
	}()

	reply.Err = ErrWrongGroup

	newOp := Op{}
	if args.DoHash {
		newOp.Op = 3
	} else {
		newOp.Op = 2
	}
	newOp.OpID = args.ID
	newOp.ClientID = args.ClientID
	newOp.Key = args.Key
	newOp.Value = args.Value

	kv.processKV(newOp, reply)
	return nil
}

// Respond to a Fetch request
func (kv *ShardKV) Fetch(args *FetchArgs, reply *FetchReply) error {
	for kv.recovering && !kv.dead {
		time.Sleep(10 * time.Millisecond)
	}
	for kv.sending && args.Sender != kv.sendingTo {
		time.Sleep(10 * time.Millisecond)
	}
//	return kv.fetchHandler(args, reply)
	return nil
}

// Respond to acknowledgement that Fetch is complete
func (kv *ShardKV) FetchComplete(args *FetchArgs, reply *FetchReply) error {
	//if args.Sender == kv.sendingTo {
	kv.sending = false
	kv.sendingTo = ""
	DPrintf("\n%v.%v: Marking sending complete", kv.gid, kv.me)
	reply.Complete = true
	//}
	return nil
}

//
// Ask the shardmaster if there's a new configuration;
// if so, re-configure.
//
func (kv *ShardKV) tick() {
	for (kv.recovering || kv.sending) && !kv.dead {
		time.Sleep(10 * time.Millisecond)
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()

	// Process any missed log entries
	seq := kv.px.Max() + 1
	kv.processLog(seq)

	// Check if current config is latest config
	newConfig := kv.sm.Query(kv.config.Num + 1)
	if newConfig.Num == kv.config.Num {
		return
	}

	DPrintf("%d.%d.%d) Found New Config: %d -> %d\n", kv.gid, kv.me, kv.config.Num, kv.config.Shards, newConfig.Shards)

	var gained []int
	var remoteGained []int
	var lost []int

	// Determine which shards I lost and which shards I gained
	for k, v := range newConfig.Shards {
		if kv.config.Shards[k] == kv.gid && v != kv.gid {
			lost = append(lost, k)
		} else if kv.config.Shards[k] != kv.gid && v == kv.gid {
			gained = append(gained, k)
			if kv.config.Shards[k] > 0 {
				remoteGained = append(remoteGained, k)
			}
		}
	}

	// Get store data and response data for new shards
	if len(remoteGained) != 0 && !kv.dead {
		DPrintf("%d.%d.%d) New Config needs %d\n", kv.gid, kv.me, kv.config.Num, remoteGained)
		for _, shard := range remoteGained {
			otherGID := kv.config.Shards[shard]
			servers := kv.config.Groups[otherGID]
			haveShard := false
			// Keep trying to get new data until success
			for !kv.dead && !haveShard {
				for sid, srv := range servers {
					keysReceived := make(map[string]bool)
					numTries := 0
					badResponse := false
					// Keep getting data until entire shard is transferred
					for !kv.dead && !haveShard && !badResponse {
						if len(keysReceived) > 0 {
							//fmt.Printf("\nAsking for more!")
						}
						DPrintf("%d.%d.%d) Attempting to get Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
						fmt.Printf("\n%d.%d.%d) Attempting to get Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
						args := &FetchArgs{newConfig.Num, shard, keysReceived, fmt.Sprintf("%v-%v", kv.gid, kv.me)}
						var reply FetchReply
						ok := call(srv, "ShardKV.Fetch", args, &reply, kv.network)
						if ok && (reply.Err == OK) {
							DPrintf("%d.%d.%d) Got Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
							//fmt.Printf("\n%d.%d.%d) Got Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
							for k, v := range reply.Store {
								kv.putValue(k, v)
								keysReceived[k] = true
							}
							for clientID, value := range reply.Response {
								kv.putResponse(-1, clientID, value)
							}
							for opID, _ := range reply.Seen {
								kv.putSeen(opID, true)
							}
							if reply.Complete {
								DPrintf("%d.%d.%d) Got Complete Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
								fmt.Printf("\n%d.%d.%d) Got Complete Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
								haveShard = true
								// Keep sending ack of Fetch until success
								waitChan := make(chan int)
								go func(server string) {
									ackSuccess := false
									ackArgs := &FetchArgs{}
									ackArgs.Sender = fmt.Sprintf("%v-%v", kv.gid, kv.me)
									var ackReply FetchReply
									waitChan <- 1
									for !kv.dead && !ackSuccess {
										DPrintf("\n%v.%v: Sending fetch complete to %s", kv.gid, kv.me, server)
										ackOK := call(server, "ShardKV.FetchComplete", ackArgs, &ackReply, kv.network)
										ackSuccess = ackOK && ackReply.Complete
										if !ackSuccess {
											time.Sleep(recoveryRetryDelay * time.Millisecond)
										}
									}
									DPrintf("\n%v.%v: Done sending fetch complete to %s", kv.gid, kv.me, server)
									//fmt.Printf("\n%v.%v: Done sending fetch complete to %s", kv.gid, kv.me, server)
								}(srv)
								<-waitChan
							}
						}
						if ok && (reply.Err != OK) && len(keysReceived) == 0 {
							DPrintf("%d.%d.%d) Failed to get Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
							badResponse = true
						}
						if !ok && numTries > 5 {
							DPrintf("%d.%d.%d) Failed to get Shard %d from %d.%d\n", kv.gid, kv.me, kv.config.Num, shard, otherGID, sid)
							badResponse = true
							// If we are declaring it dead,
							// Send it an ack of Fetch until success
							// In case it wakes up
							waitChan := make(chan int)
							go func(server string) {
								ackSuccess := false
								ackArgs := &FetchArgs{}
								ackArgs.Sender = fmt.Sprintf("%v-%v", kv.gid, kv.me)
								var ackReply FetchReply
								waitChan <- 1
								// Wait until outer loop moves on from this server
								// (should be very quick)
								for srv == server {
									time.Sleep(10 * time.Millisecond)
								}
								// Keep sending ack until success or until outer loop
								// decides to try this peer again
								for !kv.dead && !ackSuccess && (srv != server) {
									DPrintf("\n%v.%v: Sending fetch complete to %s", kv.gid, kv.me, server)
									ackOK := call(server, "ShardKV.FetchComplete", ackArgs, &ackReply, kv.network)
									ackSuccess = ackOK && ackReply.Complete
									if !ackSuccess {
										time.Sleep(recoveryRetryDelay * time.Millisecond)
									}
								}
								DPrintf("\n%v.%v: Done sending fetch complete to %s", kv.gid, kv.me, server)
							}(srv)
							<-waitChan
						}
						if !ok {
							numTries++
							time.Sleep(100 * time.Millisecond)
						}
					}
				}
				time.Sleep(250 * time.Millisecond)
			}
		}
	}

	// Record the new config in memory and disk
	kv.config = newConfig
	kv.dbWriteConfigNum(kv.config.Num)
	DPrintf("%d.%d.%d) New Config adding config %v\n", kv.gid, kv.me, kv.config.Num, newConfig.Num)
}

// please don't change this function.
func (kv *ShardKV) Kill() {
	// Kill the server
	DPrintfPersist("\n%v-%v: Killing the server", kv.gid, kv.me)
	kv.dead = true
	if kv.l != nil {
		kv.l.Close()
	}
	kv.px.Kill()

	// Close the database
	if persistent && !kv.dbClosed {
		kv.dbLock.Lock()
		kv.db.Close()
		kv.dbReadOptions.Close()
		kv.dbWriteOptions.Close()
		kv.dbLock.Unlock()
		kv.dbClosed = true
	}

	// Destroy the database
	if persistent && !kv.dbDeleted {
		DPrintfPersist("\n%v-%v: Destroying database... ", kv.gid, kv.me)
		err := levigo.DestroyDatabase(kv.dbName, kv.dbOpts)
		if err != nil {
			DPrintfPersist("\terror")
		} else {
			DPrintfPersist("\tsuccess")
			kv.dbDeleted = true
		}
	}
}

func (kv *ShardKV) KillSaveDisk() {
	// Kill the server
	DPrintfPersist("\n%v-%v: Killing the server", kv.gid, kv.me)
	kv.dead = true
	if kv.l != nil {
		kv.l.Close()
	}
	kv.px.KillSaveDisk()

	// Close the database
	if persistent && !kv.dbClosed {
		kv.dbLock.Lock()
		kv.db.Close()
		kv.dbReadOptions.Close()
		kv.dbWriteOptions.Close()
		kv.dbLock.Unlock()
		kv.dbClosed = true
	}
}

// Tries to get the value from the database
// If it doesn't exist, returns empty string
func (kv *ShardKV) dbGet(key string) (string, bool) {
	if !persistent {
		return "", false
	}
	DPrintfPersist("\n%v-%v: dbGet Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbGet Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbGet Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return "", false
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Reading value for %v from database... ", kv.gid, kv.me, key)
	// Read entry from database if it exists
	key = fmt.Sprintf("KVkey_%v", key)
	entryBytes, err := kv.db.Get(kv.dbReadOptions, []byte(key))

	// Decode the entry if it exists, otherwise return empty
	if err == nil && len(entryBytes) > 0 {
		toPrint += "\tDecoding entry... "
		buffer := *bytes.NewBuffer(entryBytes)
		decoder := gob.NewDecoder(&buffer)
		var entryDecoded string
		err = decoder.Decode(&entryDecoded)
		if err != nil {
			toPrint += "\terror"
		} else {
			toPrint += "\tsuccess"
			DPrintfPersist(toPrint)
			return entryDecoded, true
		}
	} else {
		toPrint += fmt.Sprintf("\tNo entry found in database %s", fmt.Sprint(err))
		DPrintfPersist(toPrint)
		return "", false
	}

	DPrintfPersist(toPrint)
	return "", false
}

// Writes the given key/value to the database
func (kv *ShardKV) dbPut(key string, value string) {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbPut Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbPut Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbPut Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Writing (%v, %v) to database... ", kv.gid, kv.me, key, value)
	// Encode the value into a byte array
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(value)
	if err != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(err))
	} else {
		// Write the state to the database
		key := fmt.Sprintf("KVkey_%v", key)
		err := kv.db.Put(kv.dbWriteOptions, []byte(key), buffer.Bytes())
		if err != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)
}

// Tries to get whether the given ID has been seen
func (kv *ShardKV) dbGetSeen(opID int64) bool {
	if !persistent {
		return false
	}
	DPrintfPersist("\n%v-%v: dbGetSeen Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbGetSeen Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbGetSeen Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return false
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Reading seen %v from database... ", kv.gid, kv.me, opID)
	// Read entry from database if it exists
	key := fmt.Sprintf("seen_%v", opID)
	entryBytes, err := kv.db.Get(kv.dbReadOptions, []byte(key))

	// Decode the entry if it exists, otherwise return empty
	if err == nil && len(entryBytes) > 0 {
		toPrint += "\tDecoding entry... "
		buffer := *bytes.NewBuffer(entryBytes)
		decoder := gob.NewDecoder(&buffer)
		var entryDecoded int
		err = decoder.Decode(&entryDecoded)
		if err != nil {
			toPrint += "\terror"
		} else {
			toPrint += "\tsuccess"
			DPrintfPersist(toPrint)
			return (entryDecoded == 1)
		}
	} else {
		toPrint += fmt.Sprintf("\tNo entry found in database %s", fmt.Sprint(err))
		DPrintfPersist(toPrint)
		return false
	}

	DPrintfPersist(toPrint)
	return false
}

// Writes the given client response to the database
func (kv *ShardKV) dbWriteSeen(opID int64, seen bool) {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbWriteSeen Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbWriteSeen Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbWriteSeen Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Writing seen %v -> %v to database... ", kv.gid, kv.me, opID, seen)
	// Encode the response into a byte array
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	seenVal := 1
	if !seen {
		seenVal = 0
	}
	err := enc.Encode(seenVal)
	if err != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(err))
	} else {
		// Write the state to the database
		key := fmt.Sprintf("seen_%v", opID)
		err := kv.db.Put(kv.dbWriteOptions, []byte(key), buffer.Bytes())
		if err != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)
}

// Tries to get the desired response from the database
// If it doesn't exist, returns empty string
func (kv *ShardKV) dbGetResponse(opID int64, clientID int64) (string, bool) {
	if !persistent {
		return "", false
	}
	DPrintfPersist("\n%v-%v: dbGetResponse Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbGetResponse Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbGetResponse Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return "", false
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Reading response %v (client %v) from database... ", kv.gid, kv.me, opID, clientID)
	// Return false if opID has not been seen
	seenKey := fmt.Sprintf("seen_%v", opID)
	seenBytes, seenErr := kv.db.Get(kv.dbReadOptions, []byte(seenKey))
	if seenErr != nil || len(seenBytes) == 0 {
		toPrint += fmt.Sprintf("\topID has not been seen")
		DPrintfPersist(toPrint)
		return "", false
	}

	// Read entry from database if it exists
	key := fmt.Sprintf("response_%v", clientID)
	entryBytes, err := kv.db.Get(kv.dbReadOptions, []byte(key))

	// Decode the entry if it exists, otherwise return empty
	if err == nil && len(entryBytes) > 0 {
		toPrint += "\tDecoding entry... "
		buffer := *bytes.NewBuffer(entryBytes)
		decoder := gob.NewDecoder(&buffer)
		var entryDecoded string
		err = decoder.Decode(&entryDecoded)
		if err != nil {
			toPrint += "\terror"
		} else {
			toPrint += "\tsuccess"
			DPrintfPersist(toPrint)
			return entryDecoded, true
		}
	} else {
		toPrint += fmt.Sprintf("\tNo entry found in database %s", fmt.Sprint(err))
		DPrintfPersist(toPrint)
		return "", false
	}

	DPrintfPersist(toPrint)
	return "", false
}

// Writes the given client response to the database
func (kv *ShardKV) dbWriteResponse(opID int64, clientID int64, response string) {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbWriteResponse Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbWriteResponse Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbWriteResponse Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Writing response %v (client %v) -> %v to database... ", kv.gid, kv.me, opID, clientID, response)
	// Write the response for clientID
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(response)
	if err != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(err))
	} else {
		// Write the state to the database
		key := fmt.Sprintf("response_%v", clientID)
		err := kv.db.Put(kv.dbWriteOptions, []byte(key), buffer.Bytes())
		if err != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)

	// Write that opID has been seen
	var seenBuffer bytes.Buffer
	seenEnc := gob.NewEncoder(&seenBuffer)
	seenErr := seenEnc.Encode(1)
	if seenErr != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(seenErr))
	} else {
		// Write the state to the database
		key := fmt.Sprintf("seen_%v", opID)
		seenErr := kv.db.Put(kv.dbWriteOptions, []byte(key), seenBuffer.Bytes())
		if seenErr != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)
}

// Writes the min sequence number to the database
func (kv *ShardKV) dbWriteMinSeq(seq int) {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbWriteMinSeq Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbWriteMinSeq Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbWriteMinSeq Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Writing min sequence num %v to database... ", kv.gid, kv.me, seq)
	// Encode the number into a byte array
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(seq)
	if err != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(err))
	} else {
		// Write the state to the database
		key := "minSeq"
		err := kv.db.Put(kv.dbWriteOptions, []byte(key), buffer.Bytes())
		if err != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)
}

// Writes the config number to the database
func (kv *ShardKV) dbWriteConfigNum(configNum int) {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbWriteConfigNum Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbWriteConfigNum Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbWriteConfigNum Released dbLock", kv.gid, kv.me)
	}()
	if kv.dead {
		return
	}

	toPrint := ""
	toPrint += fmt.Sprintf("\n%v-%v: Writing config num %v to database... ", kv.gid, kv.me, configNum)
	// Encode the number into a byte array
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(configNum)
	if err != nil {
		DPrintfPersist("\terror encoding: %s", fmt.Sprint(err))
	} else {
		// Write the state to the database
		key := "configNum"
		err := kv.db.Put(kv.dbWriteOptions, []byte(key), buffer.Bytes())
		if err != nil {
			toPrint += fmt.Sprintf("\terror writing to database")
		} else {
			toPrint += fmt.Sprintf("\tsuccess")
		}
	}
	DPrintfPersist(toPrint)
}

// Initialize database for persistence
// and load any previously written 'minSeq' and 'configNum' state
func (kv *ShardKV) dbInit() {
	if !persistent {
		return
	}
	DPrintfPersist("\n%v-%v: dbInit Waiting for dbLock", kv.gid, kv.me)
	kv.dbLock.Lock()
	DPrintfPersist("\n%v-%v: dbInit Got dbLock", kv.gid, kv.me)
	defer func() {
		kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: dbInit Released dbLock", kv.gid, kv.me)
	}()
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.dead {
		return
	}

	DPrintfPersist("\n%v-%v: Initializing database", kv.gid, kv.me)

	// Set up database options
	kv.dbOpts = levigo.NewOptions()
	if dbUseCache {
		if dbCacheSize*1000000 > MaxInt {
			fmt.Printf("\nDesired cache size %v is too large... using %v instead\n", dbCacheSize*1000000, MaxInt)
			kv.dbOpts.SetCache(levigo.NewLRUCache(MaxInt))
		} else {
			kv.dbOpts.SetCache(levigo.NewLRUCache(dbCacheSize * 1000000))
		}
	}
	if dbUseCompression {
		kv.dbOpts.SetCompression(levigo.SnappyCompression)
	} else {
		kv.dbOpts.SetCompression(levigo.NoCompression)
	}
	kv.dbOpts.SetCreateIfMissing(true)
	dbDir := "/home/ubuntu/mexos/src/shardkv/persist/"
	kv.dbName = dbDir + "shardkvDB_" + fmt.Sprint(kv.gid) + "_" + strconv.Itoa(kv.me)
	os.MkdirAll(dbDir, 0777)
	DPrintfPersist("\n\t%v-%v: DB Name: %s", kv.gid, kv.me, kv.dbName)
	// Open database (create it if it doesn't exist)
	var err error
	kv.db, err = levigo.Open(kv.dbName, kv.dbOpts)
	enableLog() //need this here to fix logging issues
	if err != nil {
		DPrintfPersist("\n\t%v-%v: Error opening database! \n\t%s", kv.gid, kv.me, fmt.Sprint(err))
		fmt.Printf("\n\t%v-%v: Error opening database! \n\t%s", kv.gid, kv.me, fmt.Sprint(err))
	} else {
		DPrintfPersist("\n\t%v-%v: Database opened successfully", kv.gid, kv.me)
	}

	// Create options for reading/writing entries
	kv.dbReadOptions = levigo.NewReadOptions()
	kv.dbWriteOptions = levigo.NewWriteOptions()
	kv.dbReadOptions.SetFillCache(dbUseCache)

	// Read minSeq from database if it exists
	minSeqBytes, err := kv.db.Get(kv.dbReadOptions, []byte("minSeq"))
	if err == nil && len(minSeqBytes) > 0 {
		// Decode the max instance
		DPrintfPersist("\n\t%v-%v: Decoding min seqeunce... ", kv.gid, kv.me)
		bufferMinSeq := *bytes.NewBuffer(minSeqBytes)
		decoder := gob.NewDecoder(&bufferMinSeq)
		var minSeqDecoded int
		err = decoder.Decode(&minSeqDecoded)
		if err != nil {
			DPrintfPersist("\terror decoding: %s", fmt.Sprint(err))
		} else {
			kv.minSeq = minSeqDecoded
			DPrintfPersist("\tsuccess")
		}
	} else {
		DPrintfPersist("\n\t%v-%v: No stored min sequence to load", kv.gid, kv.me)
	}

	// Read config number from database if it exists
	configNumBytes, err := kv.db.Get(kv.dbReadOptions, []byte("configNum"))
	if err == nil && len(configNumBytes) > 0 {
		// Decode the max instance
		DPrintfPersist("\n\t%v-%v: Decoding config num... ", kv.gid, kv.me)
		bufferConfigNum := *bytes.NewBuffer(configNumBytes)
		decoder := gob.NewDecoder(&bufferConfigNum)
		var configNumDecoded int
		err = decoder.Decode(&configNumDecoded)
		if err != nil {
			DPrintfPersist("\terror decoding: %s", fmt.Sprint(err))
		} else {
			kv.config = kv.sm.Query(configNumDecoded)
			if kv.config.Num != configNumDecoded {
				kv.dbLock.Unlock()
				kv.dbWriteConfigNum(kv.config.Num)
				kv.dbLock.Lock()
			}
			DPrintfPersist("\tsuccess")
		}
	} else {
		DPrintfPersist("\n\t%v-%v: No stored config num to load", kv.gid, kv.me)
	}
}

func (kv *ShardKV) maybeRecover() error {
	defer func() {
		kv.recovering = false
		log.Printf("\n%v-%v Marked recovery false", kv.gid, kv.me)
	}()
	// Initialize database, check if state is stored
	kv.recovering = true
	log.Printf("\n%v-%v Marked recovery true", kv.gid, kv.me)
	kv.dbInit()
	if !recovery {
		return nil
	}
	// Get minSeq and configNum from the most updated peer that responds
	if len(kv.servers) == 1 {
		return nil
	}	

	finished := false
	for !kv.dead && !finished {
		index := rand.Int()%len(kv.servers)
		serverToTry := kv.servers[index]
		if index == kv.me {
			continue
		}
		DPrintfPersist("\n\t%v-%v: Asking %v for kv recovery state",
			       kv.gid, kv.me, index)
		finished = kv.doRecover(serverToTry)
	}
	if finished {
		return nil
	} else {
		return errors.New("Did not recover due to death")
	}
}

func (kv *ShardKV) doRecover(serverToTry string) bool {
	ln, err := net.Listen("tcp", ":" + strconv.Itoa(recoveryPortNum))
	if err != nil {
		log.Fatal("Can not listen on recovery port", err)
	}
	defer ln.Close()

	args := RecoverArgs{kv.servers[kv.me] + ":" + strconv.Itoa(recoveryPortNum), -1, false, ""}

	finished := false

	for !kv.dead && !finished {
		var reply RecoverReply
		ok := call(serverToTry, "ShardKV.FetchRecovery", args, &reply, kv.network)
		if !ok || reply.Err {
			if args.Resume {
				DPrintfPersist("\n\t%v%v: Failed resuming recovery",
					kv.gid, kv.me)
				args := RecoverDoneArgs{}
				var reply RecoverDoneReply
				call(serverToTry, "ShardKV.RecoverDone", args, &reply, kv.network)
			}
			return false
		}
		DPrintfPersist("\n\t%v%v: Got %v", kv.gid, kv.me, reply)
		kv.config = reply.CurrentConfig
		kv.minSeq = reply.MinSeq
		kv.dbWriteMinSeq(kv.minSeq)
		kv.dbWriteConfigNum(kv.config.Num)

		c, err := ln.Accept()
		if err != nil {
			DPrintfPersist("\n\t%v%v: Failed accepting recovery connection",
				kv.gid, kv.me)
			args := RecoverDoneArgs{}
			var reply RecoverDoneReply
			call(serverToTry, "ShardKV.RecoverDone", args, &reply, kv.network)
			return false
		}

		// from now on, relooping means resumption
		args.Resume = true

		reader := bufio.NewReader(c)
		buffer := make([]byte, 1024)

	readloop:
		for !kv.dead && !finished {
			command, err := reader.ReadByte()
			if err != nil {
				DPrintfPersist("\n\t%v%v: Recovery read error",
					kv.gid, kv.me)
				c.Close()
				break readloop
			}
			switch command {
			case 0 : // done
				c.Close()
				finished = true
			case 1 : // key/value pair
				keylen, _ := binary.ReadVarint(reader)
				if int64(len(buffer)) < keylen {
					buffer = make([]byte, keylen)
				}
				io.ReadFull(reader, buffer[:keylen])
				key := string(buffer[:keylen])

				vallen, _ := binary.ReadVarint(reader)
				if int64(len(buffer)) < vallen {
					buffer = make([]byte, vallen)
				}
				_, err := io.ReadFull(reader, buffer[:vallen])
				val := string(buffer[:vallen])
			
				if (err != nil) {
					DPrintfPersist("\n\t%v%v: Recovery read error",
						kv.gid, kv.me)
					c.Close()
					break readloop
					// this goes back to resuming recovery
				}
			
				kv.putValue(key, val)
				args.LastKey = key
			}
		}


	}

	DPrintfPersist("\n\t%v%v: Succeeded in recovery",
		kv.gid, kv.me)
	rargs := RecoverDoneArgs{}
	var reply RecoverDoneReply
	call(serverToTry, "ShardKV.RecoverDone", rargs, &reply, kv.network)
	return true
}

func (kv *ShardKV) FetchRecovery(args *RecoverArgs, reply *RecoverReply) error {
	if !args.Resume {
		kv.mu.Lock()
	}
	// unlock is in RecoverDone
	DPrintfPersist("\n%v-%v: Got Fetch Recovery request", kv.gid, kv.me)

	reply.CurrentConfig = shardmaster.Config{}
	reply.CurrentConfig.Num = kv.config.Num
	reply.CurrentConfig.Groups = make(map[int64][]string)
	for gid, servers := range kv.config.Groups {
		reply.CurrentConfig.Groups[gid] = servers
	}
	for shard, gid := range kv.config.Shards {
		reply.CurrentConfig.Shards[shard] = gid
	}

	reply.MinSeq = kv.minSeq

	reply.Err = false

	go func () {
		conn, err := net.Dial("tcp", args.Address)
		if err != nil {
			DPrintfPersist("\n%v-%v: Couldn't connect to recovering servar %s",
				kv.gid, kv.me, args.Address)
			return
		}
		writer := bufio.NewWriter(conn)
		
		iterator := kv.db.NewIterator(kv.dbReadOptions)
		if args.Resume {
			iterator.Seek([]byte(args.LastKey))
			// then go past the last seen key
			iterator.Next()
		} else {
			iterator.SeekToFirst()
		}

		DPrintfPersist("\n%v-%v: Waiting for dbLock for recovery", kv.gid, kv.me)
		kv.dbLock.Lock()
		defer kv.dbLock.Unlock()
		DPrintfPersist("\n%v-%v: Got dbLock for recovery", kv.gid, kv.me)

		intbuf := make([]byte, 16)

		for iterator.Valid() {
			keyBytes := iterator.Key()
			key := string(keyBytes)

			if (args.ShardNum == -1 || args.ShardNum == key2shard(key)) {
				writer.WriteByte(1)

				n := binary.PutVarint(intbuf, int64(len(keyBytes)))
				writer.Write(intbuf[:n])
				writer.Write(keyBytes)

				valBytes := iterator.Value()
				n = binary.PutVarint(intbuf, int64(len(valBytes)))
				writer.Write(intbuf[:n])
				_, err := writer.Write(valBytes)

				if err != nil {
					DPrintfPersist("\n%v-%v: Connection error when recovering: %v",
						kv.gid, kv.me, err)
					conn.Close()
					// keep lock held for recovery reattempt
					return
				}
			}

			iterator.Next()
		}

		writer.WriteByte(0)
		writer.Flush()
		conn.Close()	
	}()

	return nil
}

func (kv *ShardKV) RecoverDone(args *RecoverDoneArgs, reply *RecoverDoneReply) error {
	DPrintfPersist("\n%v%v: RecoverDone", kv.gid, kv.me)
	kv.mu.Unlock()
	return nil
}

//
// Start a shardkv server.
// gid is the ID of the server's replica group.
// shardmasters[] contains the ports of the
//   servers that implement the shardmaster.
// servers[] contains the ports of the servers
//   in this replica group.
// Me is the index of this server in servers[].
//
func StartServer(gid int64, shardmasters []string,
	servers []string, me int, network bool) *ShardKV {
	gob.Register(Op{})

	var err error
	if Log == 1 {
		//set up logging
		os.Remove("shardkv.log")
		logfile, err = os.OpenFile("shardkv.log", os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_SYNC, 0666)

		if err != nil {
			log.Fatalf("error opening file: %v", err)
		} else {
			log.Printf("opened shardkv.log for logging")
		}
		enableLog()
	}

	//fmt.Println("running shardkv.StartServer(), network = ",network)

	kv := new(ShardKV)
	// Network stuff
	kv.me = me
	kv.network = network
        kv.servers = servers

	DPrintf("about to query for new config\n")

	// ShardKV state
	kv.gid = gid
	kv.sm = shardmaster.MakeClerk(shardmasters, kv.network)
	kv.config = kv.sm.Query(0) //hangs here, since shardmaster doesn't work
	DPrintf("got new config\n")
	kv.minSeq = -1

	// Peristence stuff
	err = kv.maybeRecover()
	if err != nil {
		log.Fatal("Recovery error: ", err)
	}

	rpcs := rpc.NewServer()
	if !printRPCerrors {
		disableLog()
		rpcs.Register(kv)
		enableLog()
	} else {
		rpcs.Register(kv)
	}

	// Give paxos a tag which is different for each group
	kv.px = paxos.Make(servers, me, rpcs, kv.network, "shardkv_"+fmt.Sprint(kv.gid))

	if kv.network {
		port := servers[me][len(servers[me])-5 : len(servers[me])]
		log.Printf("I am peers[%d] = %s, about to listen on port %s\n", me,
			servers[me], port)
		l, e := net.Listen("tcp", port)
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		kv.l = l
	} else {
		os.Remove(servers[me])
		l, e := net.Listen("unix", servers[me])
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		kv.l = l
	}

	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for kv.dead == false {
			conn, err := kv.l.Accept()
			if err == nil && kv.dead == false {
				if kv.unreliable && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if kv.unreliable && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					if !kv.network {
						c1 := conn.(*net.UnixConn)
						f, _ := c1.File()
						err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
						if err != nil {
							fmt.Printf("shutdown: %v\n", err)
						}
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && kv.dead == false {
				fmt.Printf("ShardKV(%v) accept: %v\n", me, err.Error())
				kv.Kill()
			}
		}
	}()

	go func() {
		for kv.dead == false {
			kv.tick()
			time.Sleep(250 * time.Millisecond)
		}
	}()
	return kv
}

// Returns the number of KB currently used by program memory
func getMemoryUsage() int {
	runtime.GC()
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return int(memStats.Alloc / 1024)
}

// Gets disk space used by only my shardKV databases
func (kv *ShardKV) getMyDiskUsage() int {
	return getSingleDiskUsage(kv.dbName)
}

// Gets disk space used by all shardKV databases
func getShardKVDiskUsage() int {
	return getSingleDiskUsage("/home/ubuntu/mexos/src/shardkv/persist/")
}

// Gets disk space used by all shardmaster databases
func getShardMasterDiskUsage() int {
	return getSingleDiskUsage("/home/ubuntu/mexos/src/shardmaster/persist/")
}

// Gets disk space used by all paxos databases
func getPaxosDiskUsage() int {
	return getSingleDiskUsage("/home/ubuntu/mexos/src/paxos/persist/")
}

// Gets disk space used by all paxos, shardmaster, and shardKV databases
func getDiskUsage() int {
	paxosUsage := getPaxosDiskUsage()
	shardmasterUsage := getShardMasterDiskUsage()
	shardKVUsage := getShardKVDiskUsage()
	return paxosUsage + shardmasterUsage + shardKVUsage
}

// Returns the number of KB currently used by given directory
func getSingleDiskUsage(dir string) int {
	for i := 0; i < 10; i++ {
		cmd := exec.Command("du", "-h", "-s", dir)
		cmd.Stdin = strings.NewReader("some input")
		var outBytes bytes.Buffer
		cmd.Stdout = &outBytes
		err := cmd.Run()
		if err != nil {
			//fmt.Printf("\nerror getting disk usage: %s", fmt.Sprint(err))
			time.Sleep(2 * time.Millisecond)
			continue
		}

		out := outBytes.String()
		sizeInG := false
		sizeInM := false
		sizeInK := true
		numEnd := strings.Index(out, "K")
		if numEnd < 0 {
			sizeInK = false
			sizeInM = true
			numEnd = strings.Index(out, "M")
			if numEnd < 0 {
				sizeInM = false
				sizeInG = true
				numEnd = strings.Index(out, "G")
				if numEnd < 0 {
					//fmt.Printf("\nerror getting disk usage: no size indicator: %s", out)
					time.Sleep(2 * time.Millisecond)
					continue
				}
			}
		}

		usage, err := strconv.ParseFloat(out[0:numEnd], 64)
		if err != nil {
			//fmt.Printf("\nerror getting disk usage: con't convert to float: %s (%s)", out[0:numEnd], out)
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if sizeInK {
		}
		if sizeInM {
			usage *= 1024
		}
		if sizeInG {
			usage *= 1024 * 1024
		}

		return int(usage)
	}
	return -1
}

type NullWriter int

func (NullWriter) Write([]byte) (int, error) { return 0, nil }

func enableLog() {
	if Log == 1 {
		//to file and stderr
		//log.SetOutput(io.MultiWriter(logfile, os.Stdout))
		log.SetOutput(logfile)
	} else {
		//just stderr
		log.SetOutput(os.Stdout)
	}
}

func disableLog() {
	log.SetOutput(new(NullWriter))
}
