package ethutil

import (
	"context"
	"fmt"
	"github.com/cockroachdb/errors"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"math/big"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type workState int32

var (
	timeout workState = 3
	success workState = 1
	failed  workState = 2
	ongoing workState = 0
)

type Timeout time.Duration

var (
	// TimeLess the same as context.Background(),represent has no timeout limit
	TimeLess Timeout = 1<<63 - 1
)

const maxQueryBlockSize int64 = 2000
const defaultMallocCap int64 = 1024
const maxConcurrentNumber = 1e5
const maxWorkNumber = maxConcurrentNumber / maxQueryBlockSize
const emergencyRecovery = 100
const smoothRecoverRatio = 0.25

var defaultSmoothRecoverTimes = time.Millisecond * 2

func (c *ethClient) GetCurrentBlockNumber() (uint64, error) {
	return c.client.BlockNumber(context.Background())
}

func (c *ethClient) GetEvent(timeout time.Duration, from int64, to int64, address []common.Address, topics [][]common.Hash) (stream *logsStream, err error) {
	info := newGlobalInfo(timeout, from, to, address, topics)
	var workNumber = info.workNumber
	var i int32 = 0
	for ; i < workNumber; i++ {
		newLogsWork(info).handler(c.client)
	}
	info.group.Wait()
	ok := atomic.CompareAndSwapInt32((*int32)(&info.state), 0, 1)
	if !ok {
		return nil, fmt.Errorf("get event error: %v", info.err)
	}
	logs := info.arrangeLogs()
	finalizer(info)
	stream = &logsStream{
		logs:      logs,
		client:    c,
		m:         sync.Mutex{},
		group:     sync.WaitGroup{},
		workMutex: sync.Mutex{},
	}
	return stream, nil
}
func finalizer(info *globalInfo) {
	//消除循环依赖导致的垃圾回收不了
	for i := 0; i < len(info.queue); i++ {
		info.queue[i].shareInfo = nil
	}
	info = nil
}
func (g *globalInfo) arrangeLogs() []types.Log {
	var i int32 = 0
	var result = make([]types.Log, 0, defaultMallocCap)
	for ; i < g.currentId; i++ {
		result = append(result, g.queue[i].returnValue...)
	}
	return result
}

func newGlobalInfo(timeout time.Duration, from int64, to int64, address []common.Address, topics [][]common.Hash) (g *globalInfo) {
	var workNumber = (to - from) / maxQueryBlockSize
	if maxQueryBlockSize*workNumber+from != to {
		workNumber++
	}
	g = &globalInfo{end: to, errTrigger: sync.Once{}, mutex: sync.Mutex{}, workNumber: int32(workNumber), address: address, topics: topics, offset: from, timeout: timeout, queue: make([]*logsWork, workNumber), group: sync.WaitGroup{}}
	var chanNumber = workNumber
	if chanNumber > maxWorkNumber {
		chanNumber = maxWorkNumber
	}
	g.workMutex = sync.Mutex{}
	g.controlPanel = controlPanel{cond: sync.NewCond(&g.workMutex), recoverSignal: make(chan int32, 1)}
	g.workChan = make(chan int8, chanNumber)
	var i int64
	for ; i < chanNumber; i++ {
		g.workChan <- 1
	}
	g.group.Add(int(workNumber))
	return g
}

type globalInfo struct {
	address      []common.Address
	topics       [][]common.Hash
	currentId    int32
	queue        []*logsWork
	workNumber   int32
	timeout      time.Duration
	offset       int64
	end          int64
	group        sync.WaitGroup
	state        workState  //state 0 is cantWork 1 is success 2 is failed 3 is timeout
	mutex        sync.Mutex //err mutex
	err          error
	errTrigger   sync.Once
	workMutex    sync.Mutex
	workChan     chan int8
	retryTimes   int32
	controlPanel controlPanel
	//smooth     int32
}
type logsWork struct {
	id          int32
	returnValue []types.Log
	shareInfo   *globalInfo
	done        chan struct{}
	filter      ethereum.FilterQuery
}

func newLogsWork(global *globalInfo) (result *logsWork) {
	//var barrier int32 = 0
	//atomic.LoadInt32(&barrier)
	value := atomic.AddInt32(&global.currentId, 1)
	//atomic.StoreInt32(&barrier, 1)
	id := value - 1
	end := int64(id+1)*maxQueryBlockSize - 1 + global.offset
	if end > global.end {
		end = global.end
	}
	result = &logsWork{
		id:        id,
		done:      make(chan struct{}, 1),
		shareInfo: global,
		filter:    ethereum.FilterQuery{Topics: global.topics, Addresses: global.address, FromBlock: big.NewInt(int64(id)*maxQueryBlockSize + global.offset), ToBlock: big.NewInt(end)},
	}
	result.done <- struct{}{}
	global.queue[id] = result
	return result
}

type controlPanel struct {
	cond          *sync.Cond
	state         int32 //state 0: 正常 state 1: 熔断,平滑过度
	failedTimes   int32
	sumTimes      int32
	recoverSignal chan int32
}

func (cp *controlPanel) smoothRecover() {
	fmt.Println("check control")
	defer func() {
		fmt.Println("recover")
	}()
	time.Sleep(defaultSmoothRecoverTimes)
	var timeGap = time.NewTicker(defaultSmoothRecoverTimes)
	for {
		select {
		case <-timeGap.C:
			cp.cond.Signal()
		case <-cp.recoverSignal:
			cp.cond.Broadcast()
			timeGap.Stop()
			return
		default:
		}
	}
}
func (cp *controlPanel) recover() {
	atomic.CompareAndSwapInt32(&cp.state, 1, 0)
	cp.sumTimes = 0
	cp.failedTimes = 0
	cp.recoverSignal <- 1
}

func (work *logsWork) handler(client *ethclient.Client) {
	go func() {
		<-work.shareInfo.workChan
		defer func() {
			work.shareInfo.workChan <- 0
		}()
		defer work.shareInfo.group.Done()
		state := atomic.LoadInt32((*int32)(&work.shareInfo.state))
		if state == 2 || state == 3 {
			return
		}
		//timer
		timer := time.NewTimer(work.shareInfo.timeout)
		for {
			select {
			case <-work.done:
			retryGet:
				work.shareInfo.workMutex.Lock()
				//平滑过度
				if atomic.LoadInt32(&work.shareInfo.controlPanel.state) != 0 {
					work.shareInfo.controlPanel.cond.Wait()
				}
				work.shareInfo.workMutex.Unlock()
				if atomic.LoadInt32(&work.shareInfo.controlPanel.state) == 1 {
					atomic.AddInt32(&work.shareInfo.controlPanel.sumTimes, 1)
					if atomic.LoadInt32(&work.shareInfo.controlPanel.sumTimes) == 0 || float64(work.shareInfo.controlPanel.failedTimes/work.shareInfo.controlPanel.sumTimes) < smoothRecoverRatio {
						work.shareInfo.controlPanel.recover()
					}
				}
				logs, err := client.FilterLogs(context.Background(), work.filter)
				if err != nil {
					if work.shareInfo.retryTimes >= emergencyRecovery {
						if atomic.CompareAndSwapInt32(&work.shareInfo.controlPanel.state, 0, 1) {
							fmt.Println(work.shareInfo.retryTimes)
							work.shareInfo.retryTimes = 0
							work.shareInfo.controlPanel.smoothRecover()
						}
					}
					if strings.Contains(err.Error(), "429 Too Many Requests") {
						if atomic.LoadInt32(&work.shareInfo.controlPanel.state) == 0 {
							atomic.AddInt32(&work.shareInfo.retryTimes, 1)
							time.Sleep(defaultSmoothRecoverTimes)
						} else if atomic.LoadInt32(&work.shareInfo.controlPanel.state) == 1 {
							atomic.AddInt32(&work.shareInfo.controlPanel.failedTimes, 1)
						}
						goto retryGet
					}
					//atomic.SwapInt32((*int32)(&work.state), 2)
					work.shareInfo.errTrigger.Do(func() {
						work.shareInfo.mutex.Lock()
						atomic.SwapInt32((*int32)(&work.shareInfo.state), 2)
						work.shareInfo.err = errors.New("failed")
						work.shareInfo.mutex.Unlock()
					})
					work.shareInfo.mutex.Lock()
					if work.shareInfo.err != nil {
						work.shareInfo.err = fmt.Errorf("%v \n %v", work.shareInfo.err, err)
					}
					work.shareInfo.mutex.Unlock()
					return
				}
				//atomic.SwapInt32((*int32)(&work.state), 1)
				work.returnValue = logs
				return
			case <-timer.C:
				//_ = atomic.CompareAndSwapInt32((*int32)(&work.state), 0, 3)
				work.shareInfo.mutex.Lock()
				ok := atomic.CompareAndSwapInt32((*int32)(&work.shareInfo.state), 0, 3)
				if ok {
					work.shareInfo.err = errors.New("From %s block to %s block search timeout error")
				}
				work.shareInfo.mutex.Unlock()
				return
			//monitor the global state ,in order to exit in error
			default:
				state = atomic.LoadInt32((*int32)(&work.shareInfo.state))
				if state == 2 || state == 3 {
					return
				}
			}
		}
	}()
}
