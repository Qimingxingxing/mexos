package paxos

import "testing"
import "runtime"
import "strconv"
import "os"
import "time"
import "fmt"
import "math/rand"

const onlyBenchmarks = false
const runOldTests = true
const runNewTests = true

// Make a port using the given tag and host number
func makePort(tag string, host int) string {
	s := "/var/tmp/824-"
	s += strconv.Itoa(os.Getuid()) + "/"
	os.Mkdir(s, 0777)
	s += "px-"
	s += strconv.Itoa(os.Getpid()) + "-"
	s += tag + "-"
	s += strconv.Itoa(host)
	return s
}

// Check how many of the given Paxos servers are decided on the given sequence
// Fatally errors if servers are decided on conflicting values
func numDecided(test interface{}, paxosServers []*Paxos, seq int) int {
	count := 0
	var decidedValue interface{}
	for i := 0; i < len(paxosServers); i++ {
		if paxosServers[i] != nil {
			decided, newDecidedValue := paxosServers[i].Status(seq)
			if decided {
				if count > 0 && newDecidedValue != decidedValue {
					toPrint := fmt.Sprintf("decided values do not match; seq=%v i=%v v=%v v1=%v",
						seq, i, decidedValue, newDecidedValue)
					switch test.(type) {
					case *testing.T:
						test.(*testing.T).Fatalf(toPrint)
					case *testing.B:
						test.(*testing.B).Fatalf(toPrint)
					}
				}
				count++
				decidedValue = newDecidedValue
			}
		}
	}
	return count
}

// Wait for the given number of servers to be decided
func waitForDecision(test interface{}, paxosServers []*Paxos, seq int, wanted int) {
	toWait := 10 * time.Millisecond
	for iters := 0; iters < 30; iters++ {
		if numDecided(test, paxosServers, seq) >= wanted {
			break
		}
		time.Sleep(toWait)
		if toWait < 500*time.Millisecond {
			toWait *= 2
		}
	}
	count := numDecided(test, paxosServers, seq)
	if count < wanted {
		switch test.(type) {
		case *testing.T:
			test.(*testing.T).Fatalf("too few decided; seq=%v numDecided=%v wanted=%v", seq, count, wanted)
		case *testing.B:
			test.(*testing.B).Fatalf("too few decided; seq=%v numDecided=%v wanted=%v", seq, count, wanted)
		}
	}
}

// Wait for a majority of the given servers to be decided
func waitForDecisionMajority(test interface{}, paxosServers []*Paxos, seq int) {
	waitForDecision(test, paxosServers, seq, (len(paxosServers)/2)+1)
}

// Wait for all servers to be decided
// Uses channels, so will learn of decisions immediately (no sleeping)
// Useful for benchmarking since won't add sleep time
func waitForDecisionChannels(test interface{}, paxosServers []*Paxos, seq int) {
	// Create buffered channel for responses
	doneChannel := make(chan bool, len(paxosServers))
	numFinished := 0

	// Give the channel to Paxos servers
	for i := 0; i < len(paxosServers); i++ {
		if finished, _ := paxosServers[i].Status(seq); finished {
			numFinished++
		} else {
			paxosServers[i].SetDoneChannel(seq, doneChannel)
		}
	}
	// Wait for decision
	tStart := time.Now()
	for numFinished < len(paxosServers) {
		<-doneChannel
		numFinished++
		if time.Since(tStart).Seconds() > 10 {
			switch test.(type) {
			case *testing.T:
				test.(*testing.T).Fatalf("\ntimed out waiting for decision")
			case *testing.B:
				test.(*testing.B).Fatalf("\ntimed out waiting for decision")
			}
		}
	}
	close(doneChannel)
}

// Check that there are not too many decided servers
func checkMaxDecided(test *testing.T, paxosServers []*Paxos, seq int, max int) {
	time.Sleep(3 * time.Second)
	count := numDecided(test, paxosServers, seq)
	if count > max {
		test.Fatalf("too many decided; seq=%v numDecided=%v max=%v", seq, count, max)
	}
}

// Kill the given servers
func cleanup(paxosServers []*Paxos) {
	for i := 0; i < len(paxosServers); i++ {
		if paxosServers[i] != nil {
			paxosServers[i].Kill()
		}
	}
}

// Test the Paxos agreement speed
// Waits for all servers to hear about agreements
func BenchmarkAgreementSpeed_1Instance_1Value_1Proposer(benchmark *testing.B) {

	//fmt.Printf("\nBenchmark agreement speed: single instance, single proposer, single proposal ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("time", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	benchmark.ResetTimer()
	//tStart := time.Now()
	for i := 0; i < benchmark.N; i++ {
		paxosServers[0].Start(i, "x")
		waitForDecisionChannels(benchmark, paxosServers, i)
	}
	//duration := time.Since(tStart)
	//fmt.Printf("\n\tLatency: %v us per instance", int(duration.Nanoseconds()/1000)/benchmark.N)
}

// Test the Paxos agreement speed
// Waits for all servers to hear about agreements
func BenchmarkAgreementSpeed_1Instance_5Value_1Proposer(benchmark *testing.B) {

	//fmt.Printf("\nBenchmark agreement speed: single instance, single proposer, multiple proposals ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("time", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	numValues := 5
	benchmark.ResetTimer()
	//tStart := time.Now()
	for i := 0; i < benchmark.N; i++ {
		for j := 0; j < numValues; j++ {
			go paxosServers[0].Start(i, j)
		}
		waitForDecisionChannels(benchmark, paxosServers, i)
	}
	//duration := time.Since(tStart)
	//fmt.Printf("\n\tLatency: %v us per instance", int(duration.Nanoseconds()/1000)/benchmark.N)
}

// Test the Paxos agreement speed
// Waits for all servers to hear about agreements
func BenchmarkAgreementSpeed_1Instance_5Value_3Proposer(benchmark *testing.B) {

	//fmt.Printf("\nBenchmark agreement speed: single instance, multiple proposers, multiple proposals ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("time", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	numValues := 5
	benchmark.ResetTimer()
	//tStart := time.Now()
	for i := 0; i < benchmark.N; i++ {
		for j := 0; j < numValues; j++ {
			go paxosServers[j%numServers].Start(i, j)
		}
		waitForDecisionChannels(benchmark, paxosServers, i)
	}
	//duration := time.Since(tStart)
	//fmt.Printf("\n\tLatency: %v us per instance", int(duration.Nanoseconds()/1000)/benchmark.N)
}

// Test the Paxos agreement speed
// Waits for all servers to hear about agreements
func BenchmarkAgreementSpeed_5Instance_5Value_3Proposer(benchmark *testing.B) {

	//fmt.Printf("\nBenchmark agreement speed: multiple instances, multiple proposers, multiple proposals ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("time", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	numInstances := 5
	numValues := 5
	benchmark.ResetTimer()
	//tStart := time.Now()
	for i := 0; i < benchmark.N; i++ {
		for instance := 0; instance < numInstances; instance++ {
			for j := 0; j < numValues; j++ {
				go paxosServers[j%numServers].Start(i*numInstances+instance, j)
			}
		}
		for instance := 0; instance < numInstances; instance++ {
			waitForDecisionChannels(benchmark, paxosServers, i*numInstances+instance)
		}
	}
	//duration := time.Since(tStart)
	//fmt.Printf("\n\tLatency: %v us per instance", int(duration.Nanoseconds()/1000)/benchmark.N/numInstances)
}

// Test that instances are not forgotten when servers are killed and restarted
func TestFilePersistenceBasic(test *testing.T) {
	if onlyBenchmarks || !runNewTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	tag := "persistence"
	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	defer cleanup(paxosServers)
	defer cleanPrivatePorts(tag, numServers)

	// Create ports for servers
	var paxosPorts [][]string = make([][]string, numServers)
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = make([]string, numServers)
		for j := 0; j < numServers; j++ {
			if j == i {
				// Create actual server port for myself
				paxosPorts[i][i] = makePort(tag, i)
			} else {
				// Create port that does nothing until a hard link is established by calling partitionServer()
				paxosPorts[i][j] = makePrivatePort(tag, i, j)
			}
		}
		paxosServers[i] = Make(paxosPorts[i], i, nil)
	}
	defer partitionServers(test, tag, numServers, []int{}, []int{}, []int{})

	fmt.Printf("\nTest Persistence, single failure, poke restarted server ...")
	// Put all servers in the same partition
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})
	// Get agreement on an instance (only wait for majority)
	paxosServers[0].Start(0, 0)
	waitForDecisionMajority(test, paxosServers, 0)
	// Kill one server, make sure it stops
	paxosServers[0].Kill()
	time.Sleep(1 * time.Second)
	// Get agreement on another instance
	paxosServers[1].Start(1, 1)
	waitForDecisionMajority(test, paxosServers, 1)
	// Bring server back
	paxosServers[0] = Make(paxosPorts[0], 0, nil)
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})
	// Get agreement on first instance again (poke restarted server)
	paxosServers[0].Start(0, 1)
	waitForDecision(test, paxosServers, 0, numServers)
	// See if restarted server knows about second isntance
	decided, value := paxosServers[0].Status(1)
	_, trueValue := paxosServers[1].Status(1)
	if !decided || (value != trueValue) {
		test.Fatalf("Restarted server did not learn about missed instance even after being poked")
	}

	fmt.Printf("\n\t... Passed")
	// Do it again but without poking the restarted server
	// (see if it automatically catches itself up on startup - this is needed for shardmaster query to be up to date)
	fmt.Printf("\nTest Persistence, single failure, no poke ...")

	// Kill one server, make sure it stops
	paxosServers[0].Kill()
	time.Sleep(1 * time.Second)
	// Get agreement on another instance
	paxosServers[1].Start(2, 2)
	waitForDecisionMajority(test, paxosServers, 1)
	// Bring server back
	paxosServers[0] = Make(paxosPorts[0], 0, nil)
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})
	// See if restarted server knows about missed instance
	decided, value = paxosServers[0].Status(2)
	_, trueValue = paxosServers[1].Status(2)
	if !decided || (value != trueValue) {
		test.Fatalf("Restarted server did not learn about missed instance without being poked")
	}
	fmt.Printf("\n\tPassed")
}

// Test that instances are not forgotten when majority partition is restarted
func TestFilePersistencePartition(test *testing.T) {
	if onlyBenchmarks || !runNewTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	tag := "persistencePartition"
	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	defer cleanup(paxosServers)
	defer cleanPrivatePorts(tag, numServers)

	// Create ports for servers
	var paxosPorts [][]string = make([][]string, numServers)
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = make([]string, numServers)
		for j := 0; j < numServers; j++ {
			if j == i {
				// Create actual server port for myself
				paxosPorts[i][i] = makePort(tag, i)
			} else {
				// Create port that does nothing until a hard link is established by calling partitionServer()
				paxosPorts[i][j] = makePrivatePort(tag, i, j)
			}
		}
		paxosServers[i] = Make(paxosPorts[i], i, nil)
	}
	defer partitionServers(test, tag, numServers, []int{}, []int{}, []int{})

	fmt.Printf("\nTest Persistence, partition ...")
	// Partition servers
	partitionServers(test, tag, numServers, []int{0, 1, 2}, []int{3, 4}, []int{})
	// Get agreement in the majority partition
	paxosServers[0].Start(0, 0)
	waitForDecisionMajority(test, paxosServers, 0)
	_, decidedValue := paxosServers[0].Status(0)
	// Kill the servers in the majority partition
	paxosServers[0].Kill()
	paxosServers[1].Kill()
	paxosServers[2].Kill()
	time.Sleep(1 * time.Second)
	// Start instance in minority
	paxosServers[3].Start(0, 1)
	time.Sleep(1 * time.Second)
	// Bring majority back
	paxosServers[0] = Make(paxosPorts[0], 0, nil)
	paxosServers[1] = Make(paxosPorts[1], 1, nil)
	paxosServers[2] = Make(paxosPorts[2], 2, nil)
	// Check that old value is forced when partition heals
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})
	waitForDecisionMajority(test, paxosServers, 0)
	_, newValue := paxosServers[3].Status(0)
	if decidedValue != newValue {
		test.Fatalf("Decided value changed when majority partition restarted")
	}
	fmt.Printf("\n\tPassed")
}

// Test that instances are not forgotten when all servers are killed and restarted
func TestFilePersistenceAllRestart(test *testing.T) {
	if onlyBenchmarks || !runNewTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	tag := "persistenceAll"
	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	defer cleanup(paxosServers)
	defer cleanPrivatePorts(tag, numServers)

	// Create ports for servers
	var paxosPorts [][]string = make([][]string, numServers)
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = make([]string, numServers)
		for j := 0; j < numServers; j++ {
			if j == i {
				// Create actual server port for myself
				paxosPorts[i][i] = makePort(tag, i)
			} else {
				// Create port that does nothing until a hard link is established by calling partitionServer()
				paxosPorts[i][j] = makePrivatePort(tag, i, j)
			}
		}
		paxosServers[i] = Make(paxosPorts[i], i, nil)
	}
	defer partitionServers(test, tag, numServers, []int{}, []int{}, []int{})

	fmt.Printf("\nTest Persistence, all servers restart ...")
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})

	// Get agreement on instance
	paxosServers[0].Start(0, 0)
	waitForDecision(test, paxosServers, 0, numServers)
	// Call Done on all but one server
	for i := 1; i < numServers; i++ {
		paxosServers[i].Done(0)
	}
	// Get agreement on some more instances
	paxosServers[1].Start(1, 1)
	paxosServers[2].Start(2, 2)
	waitForDecision(test, paxosServers, 1, numServers)
	waitForDecision(test, paxosServers, 2, numServers)
	_, decidedValue := paxosServers[2].Status(0)
	// Kill all servers
	for i := 0; i < numServers; i++ {
		paxosServers[i].Kill()
	}
	time.Sleep(2 * time.Second)
	// Restart all servers
	// As each is started, check min and old instance
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts[i], i, nil)
		min := paxosServers[i].Min()
		max := paxosServers[i].Max()
		_, recoveredValue := paxosServers[i].Status(0)
		if min != 0 || max != 2 {
			test.Fatalf("Restarted servers forgot instances")
		}
		if recoveredValue != decidedValue {
			test.Fatalf("Restarted server forgot decided value")
		}
	}
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})
	time.Sleep(1 * time.Second)
	// Call Done on server that was left out before
	paxosServers[0].Done(0)
	// Get agreement on new instance
	paxosServers[0].Start(0, 3)
	waitForDecision(test, paxosServers, 0, numServers)
	// Check that Min advanced
	for i := 0; i < numServers; i++ {
		min := paxosServers[i].Min()
		if min != 1 {
			test.Fatalf("Restarted servers did not remember their Done values so Min did not advance")
		}
	}

	fmt.Printf("\n\tPassed")
}

// Test that servers can propose for any sequence and agreement is reached
func TestFileBasic(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for the servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("basic", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	fmt.Printf("\nTest: Single proposer ...")

	paxosServers[0].Start(0, "hello")
	waitForDecision(test, paxosServers, 0, numServers)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: Many proposers, same value ...")

	for i := 0; i < numServers; i++ {
		paxosServers[i].Start(1, 77)
	}
	waitForDecision(test, paxosServers, 1, numServers)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: Many proposers, different values ...")

	paxosServers[0].Start(2, 100)
	paxosServers[1].Start(2, 101)
	paxosServers[2].Start(2, 102)
	waitForDecision(test, paxosServers, 2, numServers)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: Out-of-order instances ...")

	paxosServers[0].Start(7, 700)
	paxosServers[0].Start(6, 600)
	paxosServers[1].Start(5, 500)
	waitForDecision(test, paxosServers, 7, numServers)
	paxosServers[0].Start(4, 400)
	paxosServers[1].Start(3, 300)
	waitForDecision(test, paxosServers, 6, numServers)
	waitForDecision(test, paxosServers, 5, numServers)
	waitForDecision(test, paxosServers, 4, numServers)
	waitForDecision(test, paxosServers, 3, numServers)

	if paxosServers[0].Max() != 7 {
		test.Fatalf("wrong Max()")
	}

	fmt.Printf("\n\tPassed")
}

func TestFileDeaf(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("deaf", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	fmt.Printf("\nTest: Deaf proposer ...")

	// Put in initial sequence that everyone hears about
	paxosServers[0].Start(0, "hello")
	waitForDecision(test, paxosServers, 0, numServers)

	// Remove two servers and propose new sequence
	os.Remove(paxosPorts[0])
	os.Remove(paxosPorts[numServers-1])

	paxosServers[1].Start(1, "goodbye")
	waitForDecision(test, paxosServers, 1, numServers-2)
	if numDecided(test, paxosServers, 1) != numServers-2 {
		test.Fatalf("a deaf peer heard about a decision")
	}

	// Use one of the previously deaf servers to propose
	paxosServers[0].Start(1, "xxx")
	waitForDecision(test, paxosServers, 1, numServers-1)
	if numDecided(test, paxosServers, 1) != numServers-1 {
		test.Fatalf("a deaf peer heard about a decision")
	}

	// Use last deaf server to propose
	paxosServers[numServers-1].Start(1, "yyy")
	waitForDecision(test, paxosServers, 1, numServers)

	fmt.Printf("\n\tPassed")
}

// Test that old sequences are forgotten
func TestFileForget(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	const numServers = 6
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("forget", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	fmt.Printf("\nTest: Forgetting ...")

	// initial Min() correct?
	for i := 0; i < numServers; i++ {
		m := paxosServers[i].Min()
		if m > 0 {
			test.Fatalf("wrong initial Min() %v", m)
		}
	}

	// Start proposals for a few sequences
	paxosServers[0].Start(0, "00")
	paxosServers[1].Start(1, "11")
	paxosServers[2].Start(2, "22")
	paxosServers[0].Start(6, "66")
	paxosServers[1].Start(7, "77")

	waitForDecision(test, paxosServers, 0, numServers)

	// Min() correct?
	for i := 0; i < numServers; i++ {
		m := paxosServers[i].Min()
		if m != 0 {
			test.Fatalf("wrong Min() %v after deciding sequence 0; expected 0", m)
		}
	}

	waitForDecision(test, paxosServers, 1, numServers)

	// Min() correct?
	for i := 0; i < numServers; i++ {
		m := paxosServers[i].Min()
		if m != 0 {
			test.Fatalf("wrong Min() %v after deciding sequence 1; expected 0", m)
		}
	}

	// everyone Done() -> Min() changes?
	for i := 0; i < numServers; i++ {
		paxosServers[i].Done(0)
	}
	for i := 1; i < numServers; i++ {
		paxosServers[i].Done(1)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i].Start(8+i, "xx")
	}
	allok := false
	// Everyone's min() should be 1
	for iters := 0; iters < 12; iters++ {
		allok = true
		for i := 0; i < numServers; i++ {
			min := paxosServers[i].Min()
			if min != 1 {
				allok = false
			}
		}
		if allok {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if allok != true {
		test.Fatalf("Min() did not properly advance after Done()")
	}

	fmt.Printf("\n\tPassed")
}

// Test a lot of forgetting of sequences
// Unreliable communications
// TODO make this actually test something?
func TestFileManyForgetUnreliable(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("forgetMany", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
		paxosServers[i].unreliable = true
	}

	fmt.Printf("\nTest: Lots of forgetting ...")

	const maxSeq = 20
	done := false

	// Start a lot of proposals for random sequences
	go func() {
		sequenceNumbers := rand.Perm(maxSeq)
		for i := 0; i < len(sequenceNumbers); i++ {
			seq := sequenceNumbers[i]
			proposer := (rand.Int() % numServers)
			value := rand.Int()
			paxosServers[proposer].Start(seq, value)
			runtime.Gosched() // Allow other goroutines to run
		}
	}()

	// Randomly call Done on decided sequences
	go func() {
		for done == false {
			seq := (rand.Int() % maxSeq)
			serverToCheck := (rand.Int() % numServers)
			// If haven't already called Done and it's decided, call Done
			if seq >= paxosServers[serverToCheck].Min() {
				decided, _ := paxosServers[serverToCheck].Status(seq)
				if decided {
					paxosServers[serverToCheck].Done(seq)
				}
			}
			runtime.Gosched()
		}
	}()

	time.Sleep(5 * time.Second)
	done = true
	for i := 0; i < numServers; i++ {
		paxosServers[i].unreliable = false
	}
	time.Sleep(2 * time.Second)

	for seq := 0; seq < maxSeq; seq++ {
		for i := 0; i < numServers; i++ {
			if seq >= paxosServers[i].Min() {
				paxosServers[i].Status(seq)
			}
		}
	}

	fmt.Printf("\n\tPassed")
}

//
// does paxos forgetting actually free the memory?
//
func TestFileForgetMem(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Paxos frees forgotten instance memory ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("forgetMemory", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	// Run initial sequence
	paxosServers[0].Start(0, "x")
	waitForDecision(test, paxosServers, 0, numServers)

	// Check initial memory usage (should be about a megabyte)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	for i := 1; i <= 10; i++ {
		big := make([]byte, 1000000)
		for j := 0; j < len(big); j++ {
			big[j] = byte('a' + rand.Int()%26)
		}
		paxosServers[0].Start(i, string(big))
		waitForDecision(test, paxosServers, i, numServers)
	}

	// Check memory after 10 large number proposals (should be about 90 megabytes)
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	// Call Done on highest sequence to forget all big proposals
	for i := 0; i < numServers; i++ {
		paxosServers[i].Done(10)
	}
	// Propose a bunch of small sequences
	for i := 0; i < numServers; i++ {
		paxosServers[i].Start(11+i, "z")
	}
	time.Sleep(3 * time.Second)
	for i := 0; i < numServers; i++ {
		if paxosServers[i].Min() != 11 {
			test.Fatalf("expected Min() %v, got %v\n", 11, paxosServers[i].Min())
		}
	}

	// Check final memory (should be about 10 megabytes)
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	if m2.Alloc > (m1.Alloc / 2) {
		test.Fatalf("memory use did not shrink enough")
	}

	fmt.Printf("\n\tPassed")
}

// Check that RPC counts aren't too high
func TestFileRPCCountRegular(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: RPC counts aren't too high ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("count", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	numInstances := 5
	seq := 0
	for i := 0; i < numInstances; i++ {
		paxosServers[0].Start(seq, "x")
		waitForDecision(test, paxosServers, seq, numServers)
		seq++
	}

	rpcCount := 0
	for j := 0; j < numServers; j++ {
		rpcCount += paxosServers[j].rpcCount
	}

	// per agreement:
	// numServers prepares
	// numServers accepts
	// numServers decides
	rpcCountExpected := numInstances * (numServers - 1) * 3
	fmt.Printf("\n\tRPC count, single proposer: %v (expected max %v)", rpcCount, rpcCountExpected)
	if rpcCount > rpcCountExpected {
		test.Fatalf("too many RPCs for serial Start()s; %v instances, got %v, expected %v",
			numInstances, rpcCount, rpcCountExpected)
	}

	numInstances = 5
	for i := 0; i < numInstances; i++ {
		for j := 0; j < numServers; j++ {
			go paxosServers[j].Start(seq+i, j+(i*10))
		}
	}
	for i := 0; i < numInstances; i++ {
		waitForDecision(test, paxosServers, seq, numServers)
		seq++
	}

	rpcCount *= -1 // Want to subtract previous count to only count this round of RPCs
	for j := 0; j < numServers; j++ {
		rpcCount += paxosServers[j].rpcCount
	}

	// worst case per agreement:
	// Proposer 1: 3 prep, 3 acc, 3 decides.
	// Proposer 2: 3 prep, 3 acc, 3 prep, 3 acc, 3 decides.
	// Proposer 3: 3 prep, 3 acc, 3 prep, 3 acc, 3 prep, 3 acc, 3 decides.
	rpcCountExpected = numInstances * (numServers - 1) * 15
	fmt.Printf("\n\tRPC count, multiple proposers: %v (expected max %v)", rpcCount, rpcCountExpected)
	if rpcCount > rpcCountExpected {
		test.Fatalf("too many RPCs for concurrent Start()s; %v instances, got %v, expected %v",
			numInstances, rpcCount, rpcCountExpected)
	}

	fmt.Printf("\n\tPassed")
}

// Check that RPC counts aren't too high
func TestFileRPCCountPrePrepare(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Pre-prepare messages reduce RPC count ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("count", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
	}

	seq := 0

	// Perform initial agreement, to stimulate pre-prepares if necessary
	paxosServers[0].Start(seq, "x")
	waitForDecision(test, paxosServers, seq, numServers)
	seq++
	rpcCount := 0
	for j := 0; j < numServers; j++ {
		rpcCount += paxosServers[j].rpcCount
	}
	rpcCount *= -1 // Don't count these initial messages

	// Perform agreement on multiple instances, one at a time
	// with a single proposer for all of them
	numInstances := 5
	for i := 0; i < numInstances; i++ {
		paxosServers[0].Start(seq, "x")
		waitForDecision(test, paxosServers, seq, numServers)
		seq++
	}
	for j := 0; j < numServers; j++ {
		rpcCount += paxosServers[j].rpcCount
	}

	// Check rpcCount
	// per instance, need Accept and Decided messages
	// so 2 messages to each server per instance
	// messages to self are not RPC, so use (numServers - 1)
	rpcCountExpected := numInstances * (numServers - 1) * 2
	fmt.Printf("\n\tRPC count, single proposer: %v (expected max %v)", rpcCount, rpcCountExpected)
	if rpcCount > rpcCountExpected {
		test.Fatalf("too many RPCs for serial Start()s; %v instances, got %v, expected %v",
			numInstances, rpcCount, rpcCountExpected)
	}

	// Perform agreement on multiple simultaneous instances
	// with different proposers
	numInstances = 5
	for i := 0; i < numInstances; i++ {
		for j := 0; j < numServers; j++ {
			go paxosServers[j].Start(seq+i, j+(i*10))
		}
	}
	for i := 0; i < numInstances; i++ {
		waitForDecision(test, paxosServers, seq, numServers)
		seq++
	}
	rpcCount *= -1 // Want to subtract previous count to only count this round of RPCs
	for j := 0; j < numServers; j++ {
		rpcCount += paxosServers[j].rpcCount
	}

	// TODO figure out what expected count should be
	// will probably depend on chosen implementation
	fmt.Printf("RPC count: %v (expected max ?)", rpcCount)

	fmt.Printf("\n\tPassed")
}

//
// many agreements (without failures)
//
func TestFileMany(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Many instances ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("many", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
		paxosServers[i].Start(0, 0)
	}

	const numInstances = 50
	// Start a lot of sequences (but only 5 active at a time)
	for seq := 1; seq < numInstances; seq++ {
		// only 5 active instances, to limit the
		// number of file descriptors.
		for seq >= 5 && numDecided(test, paxosServers, seq-5) < numServers {
			time.Sleep(20 * time.Millisecond)
		}
		for i := 0; i < numServers; i++ {
			paxosServers[i].Start(seq, (seq*10)+i)
		}
	}

	// Wait for all instances to be decided
	for {
		done := true
		for seq := 1; seq < numInstances; seq++ {
			if numDecided(test, paxosServers, seq) < numServers {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("\n\tPassed")
}

//
// a peer starts up, with proposal, after others decide.
// then another peer starts, without a proposal.
//
func TestFileOld(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Minority proposal ignored ...")

	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("old", i)
	}

	paxosServers[1] = Make(paxosPorts, 1, nil)
	paxosServers[2] = Make(paxosPorts, 2, nil)
	paxosServers[3] = Make(paxosPorts, 3, nil)
	paxosServers[1].Start(1, 111)

	waitForDecisionMajority(test, paxosServers, 1)

	paxosServers[0] = Make(paxosPorts, 0, nil)
	paxosServers[0].Start(1, 222)

	waitForDecision(test, paxosServers, 1, 4)

	if false {
		paxosServers[4] = Make(paxosPorts, 4, nil)
		waitForDecision(test, paxosServers, 1, numServers)
	}

	fmt.Printf("\n\tPassed")
}

//
// many agreements, with unreliable RPC
//
func TestFileManyUnreliable(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Many instances, unreliable RPC ...")

	const numServers = 3
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	var paxosPorts []string = make([]string, numServers)
	defer cleanup(paxosServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		paxosPorts[i] = makePort("manyUnreliable", i)
	}
	for i := 0; i < numServers; i++ {
		paxosServers[i] = Make(paxosPorts, i, nil)
		paxosServers[i].unreliable = true
		paxosServers[i].Start(0, 0)
	}

	const numInstances = 50
	// Start a bunch of sequences, but only 3 active at a time
	for seq := 1; seq < numInstances; seq++ {
		// only 3 active instances, to limit the
		// number of file descriptors.
		for seq >= 3 && numDecided(test, paxosServers, seq-3) < numServers {
			time.Sleep(20 * time.Millisecond)
		}
		for i := 0; i < numServers; i++ {
			paxosServers[i].Start(seq, (seq*10)+i)
		}
	}

	// Wait for decisions
	for {
		done := true
		for seq := 1; seq < numInstances; seq++ {
			if numDecided(test, paxosServers, seq) < numServers {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("\n\tPassed")
}

// Make a port meant for communication between the given src and dst
func makePrivatePort(tag string, src int, dst int) string {
	s := "/var/tmp/824-"
	s += strconv.Itoa(os.Getuid()) + "/"
	s += "px-" + tag + "-"
	s += strconv.Itoa(os.Getpid()) + "-"
	s += strconv.Itoa(src) + "-"
	s += strconv.Itoa(dst)
	return s
}

// Delete all private ports created for directed communication
func cleanPrivatePorts(tag string, n int) {
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			port_ij := makePrivatePort(tag, i, j)
			os.Remove(port_ij)
		}
	}
}

// Partition the servers into the specified three partitions
func partitionServers(test *testing.T, tag string, numServers int, p1 []int, p2 []int, p3 []int) {
	// Delete any links that may have been made previously
	cleanPrivatePorts(tag, numServers)

	// Loop through desired partitions, forging the necessary communication links
	// Will link files so that servers in the same partition can read/write from each other's files
	// Servers in different partitions will try to write to files which no one is actually listening on
	partitions := [][]int{p1, p2, p3}
	for partitionIndex := 0; partitionIndex < len(partitions); partitionIndex++ {
		partition := partitions[partitionIndex]
		// Loop through all combinations of servers in this partition
		for i := 0; i < len(partition); i++ {
			for j := 0; j < len(partition); j++ {
				port_ij := makePrivatePort(tag, partition[i], partition[j])
				port_j := makePort(tag, partition[j])
				// Create hard link between the actual server port (port_j)
				// And the port which server i thinks it should use to talk to j (port_ij)
				// Thus, server i will in fact be able to talk to server j
				err := os.Link(port_j, port_ij)
				if err != nil {
					// one reason this link can fail is if the
					// corresponding Paxos peer has prematurely quit and
					// deleted its socket file (e.g., called px.Kill()).
					test.Fatalf("os.Link(%v, %v): %v\n", port_ij, port_j, err)
				}
			}
		}
	}
}

func TestFilePartition(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	tag := "partition"
	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	defer cleanup(paxosServers)
	defer cleanPrivatePorts(tag, numServers)

	// Create ports for servers
	for i := 0; i < numServers; i++ {
		var paxosPorts []string = make([]string, numServers)
		for j := 0; j < numServers; j++ {
			if j == i {
				// Create actual server port for myself
				paxosPorts[i] = makePort(tag, i)
			} else {
				// Create port that does nothing until a hard link is established by calling partitionServer()
				paxosPorts[j] = makePrivatePort(tag, i, j)
			}
		}
		paxosServers[i] = Make(paxosPorts, i, nil)
	}
	defer partitionServers(test, tag, numServers, []int{}, []int{}, []int{})

	seq := 0

	fmt.Printf("\nTest: No decision if partitioned ...")

	partitionServers(test, tag, numServers, []int{0, 2}, []int{1, 3}, []int{4})
	paxosServers[1].Start(seq, 111)
	checkMaxDecided(test, paxosServers, seq, 0)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: Decision in majority partition ...")

	partitionServers(test, tag, numServers, []int{0}, []int{1, 2, 3}, []int{4})
	waitForDecisionMajority(test, paxosServers, seq)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: All agree after full heal ...")

	paxosServers[0].Start(seq, 1000) // poke them
	paxosServers[4].Start(seq, 1004)
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})

	waitForDecision(test, paxosServers, seq, numServers)

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: One peer switches partitions ...")

	for numIterations := 0; numIterations < 20; numIterations++ {
		seq++
		partitionServers(test, tag, numServers, []int{0, 1, 2}, []int{3, 4}, []int{})
		paxosServers[0].Start(seq, seq*10)
		paxosServers[3].Start(seq, (seq*10)+1)
		waitForDecisionMajority(test, paxosServers, seq)
		if numDecided(test, paxosServers, seq) > 3 {
			test.Fatalf("too many decided")
		}

		partitionServers(test, tag, numServers, []int{0, 1}, []int{2, 3, 4}, []int{})
		waitForDecision(test, paxosServers, seq, numServers)
	}

	fmt.Printf("\n\tPassed")

	fmt.Printf("\nTest: One peer switches partitions, unreliable ...")

	for numIterations := 0; numIterations < 20; numIterations++ {
		seq++

		for i := 0; i < numServers; i++ {
			paxosServers[i].unreliable = true
		}

		partitionServers(test, tag, numServers, []int{0, 1, 2}, []int{3, 4}, []int{})
		for i := 0; i < numServers; i++ {
			paxosServers[i].Start(seq, (seq*10)+i)
		}
		waitForDecision(test, paxosServers, seq, 3)
		if numDecided(test, paxosServers, seq) > 3 {
			test.Fatalf("too many decided")
		}

		partitionServers(test, tag, numServers, []int{0, 1}, []int{2, 3, 4}, []int{})

		for i := 0; i < numServers; i++ {
			paxosServers[i].unreliable = false
		}

		waitForDecision(test, paxosServers, seq, 5)
	}

	fmt.Printf("\n\tPassed")
}

func TestFileLots(test *testing.T) {
	if onlyBenchmarks || !runOldTests {
		return
	}
	if network {
		test.Fatalf("need to unset network flag!")
	}
	runtime.GOMAXPROCS(4)

	fmt.Printf("\nTest: Many requests, changing partitions ...")

	tag := "lots"
	const numServers = 5
	var paxosServers []*Paxos = make([]*Paxos, numServers)
	defer cleanup(paxosServers)
	defer cleanPrivatePorts(tag, numServers)

	// Make ports for servers
	for i := 0; i < numServers; i++ {
		var paxosPorts []string = make([]string, numServers)
		for j := 0; j < numServers; j++ {
			if j == i {
				// Make actual server port for myself
				paxosPorts[i] = makePort(tag, i)
			} else {
				// Create port that does nothing until a hard link is established by calling partitionServer()
				paxosPorts[j] = makePrivatePort(tag, i, j)
			}
		}
		paxosServers[i] = Make(paxosPorts, i, nil)
		paxosServers[i].unreliable = true
	}
	defer partitionServers(test, tag, numServers, []int{}, []int{}, []int{})

	done := false

	// re-partition periodically
	partitionDoneChannel := make(chan bool)
	go func() {
		defer func() { partitionDoneChannel <- true }()
		for done == false {
			// Randomly assign each server to a partition
			partitions := make([][]int, 3)
			for i := 0; i < numServers; i++ {
				partition := (rand.Int() % 3)
				partitions[partition] = append(partitions[partition], i)
			}
			partitionServers(test, tag, numServers, partitions[0], partitions[1], partitions[2])
			time.Sleep(time.Duration(rand.Int63()%200) * time.Millisecond)
		}
	}()

	seq := 0

	// periodically start a new instance
	proposerDoneChannel := make(chan bool)
	go func() {
		defer func() { proposerDoneChannel <- true }()
		for done == false {
			// How many instances are in progress?
			decidedCount := 0
			for i := 0; i < seq; i++ {
				if numDecided(test, paxosServers, i) == numServers {
					decidedCount++
				}
			}
			// If less than 10 active sequences, start a new one (on every server)
			if seq-decidedCount < 10 {
				for i := 0; i < numServers; i++ {
					paxosServers[i].Start(seq, rand.Int()%10)
				}
				seq++
			}
			time.Sleep(time.Duration(rand.Int63()%300) * time.Millisecond)
		}
	}()

	// periodically check that decisions are consistent
	checkerDoneChannel := make(chan bool)
	go func() {
		defer func() { checkerDoneChannel <- true }()
		for done == false {
			// Check that all sequences are consistent
			for i := 0; i < seq; i++ {
				numDecided(test, paxosServers, i)
			}
			time.Sleep(time.Duration(rand.Int63()%300) * time.Millisecond)
		}
	}()

	// Run for 20 seconds and then kill the threads
	time.Sleep(20 * time.Second)
	done = true
	<-proposerDoneChannel
	<-partitionDoneChannel
	<-checkerDoneChannel

	// Repair partitions, then check that all instances decided.
	for i := 0; i < numServers; i++ {
		paxosServers[i].unreliable = false
	}
	partitionServers(test, tag, numServers, []int{0, 1, 2, 3, 4}, []int{}, []int{})

	for i := 0; i < seq; i++ {
		waitForDecisionMajority(test, paxosServers, i)
	}

	fmt.Printf("\n\tPassed\n\n")
}