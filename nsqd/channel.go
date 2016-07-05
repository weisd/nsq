package nsqd

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/absolute8511/nsq/internal/levellogger"
	"github.com/absolute8511/nsq/internal/quantile"
)

const (
	resetReaderTimeoutSec = 10
)

var (
	ErrMsgNotInFlight                 = errors.New("Message ID not in flight")
	ErrMsgAlreadyInFlight             = errors.New("Message ID already in flight")
	ErrConsumeDisabled                = errors.New("Consume is disabled currently")
	ErrMsgDeferred                    = errors.New("Message is deferred")
	ErrSetConsumeOffsetNotFirstClient = errors.New("consume offset can only be changed by the first consume client")
	ErrNotDiskQueueReader             = errors.New("the consume channel is not disk queue reader")
)

type Consumer interface {
	UnPause()
	Pause()
	TimedOutMessage(delayed bool)
	Stats() ClientStats
	Exit()
	Empty()
}

// Channel represents the concrete type for a NSQ channel (and also
// implements the Queue interface)
//
// There can be multiple channels per topic, each with there own unique set
// of subscribers (clients).
//
// Channels maintain all client and message metadata, orchestrating in-flight
// messages, timeouts, requeuing, etc.
type Channel struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	requeueCount  uint64
	timeoutCount  uint64
	deferredCount int64

	sync.RWMutex

	topicName  string
	topicPart  int
	name       string
	notifyCall func(v interface{})
	option     *Options

	backend BackendQueueReader

	requeuedMsgChan     chan *Message
	waitingRequeueMsgs  map[MessageID]*Message
	waitingRequeueMutex sync.Mutex
	clientMsgChan       chan *Message
	exitChan            chan int
	exitSyncChan        chan bool
	exitFlag            int32
	exitMutex           sync.RWMutex

	// state tracking
	clients        map[int64]Consumer
	paused         int32
	ephemeral      bool
	deleteCallback func(*Channel)
	deleter        sync.Once

	// Stats tracking
	e2eProcessingLatencyStream *quantile.Quantile

	inFlightMessages map[MessageID]*Message
	inFlightPQ       inFlightPqueue
	inFlightMutex    sync.Mutex

	currentLastConfirmed int64
	confirmedMsgs        map[int64]*Message
	confirmMutex         sync.Mutex
	waitingConfirm       int32
	tryReadBackend       chan bool
	readerChanged        chan BackendOffset
	endUpdatedChan       chan bool
	needNotifyRead       int32
	consumeDisabled      int32
	// stat counters
	EnableTrace int32
	//finMsgs     map[MessageID]*Message
	//finErrMsgs map[MessageID]string
	requireOrder           int32
	needResetReader        int32
	processResetReaderTime int64
	waitingProcessMsgTs    int64
}

// NewChannel creates a new instance of the Channel type and returns a pointer
func NewChannel(topicName string, part int, channelName string, opt *Options,
	deleteCallback func(*Channel), consumeDisabled int32, notify func(v interface{})) *Channel {

	c := &Channel{
		topicName:          topicName,
		topicPart:          part,
		name:               channelName,
		requeuedMsgChan:    make(chan *Message, opt.MaxRdyCount+1),
		waitingRequeueMsgs: make(map[MessageID]*Message, 100),
		clientMsgChan:      make(chan *Message),
		exitChan:           make(chan int),
		exitSyncChan:       make(chan bool),
		clients:            make(map[int64]Consumer),
		confirmedMsgs:      make(map[int64]*Message),
		//finMsgs:            make(map[MessageID]*Message),
		//finErrMsgs:     make(map[MessageID]string),
		tryReadBackend:  make(chan bool, 1),
		readerChanged:   make(chan BackendOffset, 1),
		endUpdatedChan:  make(chan bool, 1),
		deleteCallback:  deleteCallback,
		option:          opt,
		notifyCall:      notify,
		consumeDisabled: consumeDisabled,
	}
	if len(opt.E2EProcessingLatencyPercentiles) > 0 {
		c.e2eProcessingLatencyStream = quantile.New(
			opt.E2EProcessingLatencyWindowTime,
			opt.E2EProcessingLatencyPercentiles,
		)
	}
	// channel no need sync so much.
	syncEvery := opt.SyncEvery * 1000
	if syncEvery < 1 {
		syncEvery = 1
	}

	c.initPQ()

	if strings.HasSuffix(channelName, "#ephemeral") {
		c.ephemeral = true
		c.backend = newDummyBackendQueueReader()
	} else {
		// backend names, for uniqueness, automatically include the topic...
		backendReaderName := getBackendReaderName(c.topicName, c.topicPart, channelName)
		backendName := getBackendName(c.topicName, c.topicPart)
		c.backend = newDiskQueueReader(backendName, backendReaderName,
			path.Join(opt.DataPath, c.topicName),
			opt.MaxBytesPerFile,
			int32(minValidMsgLength),
			int32(opt.MaxMsgSize)+minValidMsgLength,
			syncEvery,
			opt.SyncTimeout,
			false)

		c.UpdateConfirmedOffset(c.backend.(*diskQueueReader).virtualConfirmedOffset)
	}

	go c.messagePump()

	c.notifyCall(c)

	return c
}

func (c *Channel) GetName() string {
	return c.name
}

func (c *Channel) GetTopicName() string {
	return c.topicName
}

func (c *Channel) GetTopicPart() int {
	return c.topicPart
}

func (c *Channel) GetClientMsgChan() chan *Message {
	return c.clientMsgChan
}

func (c *Channel) IsTraced() bool {
	return atomic.LoadInt32(&c.EnableTrace) == 1
}

func (c *Channel) SetTrace(enable bool) {
	if enable {
		atomic.StoreInt32(&c.EnableTrace, 1)
	} else {
		atomic.StoreInt32(&c.EnableTrace, 0)
	}
}

func (c *Channel) SetConsumeOffset(offset BackendOffset, force bool) error {
	c.Lock()
	num := len(c.clients)
	c.Unlock()
	if num > 1 && !force {
		return ErrSetConsumeOffsetNotFirstClient
	}
	_, ok := c.backend.(*diskQueueReader)
	if ok {
		select {
		case c.readerChanged <- offset:
		default:
		}
	} else {
		return ErrNotDiskQueueReader
	}
	return nil
}

func (c *Channel) SetOrdered(enable bool) {
	if enable {
		atomic.StoreInt32(&c.requireOrder, 1)
	} else {
		if c.GetClientsCount() == 0 {
			atomic.StoreInt32(&c.requireOrder, 0)
			select {
			case c.tryReadBackend <- true:
			default:
			}
		} else {
			nsqLog.Logf("can not set ordered to false while the channel is still consuming by client")
		}
	}
}

func (c *Channel) IsOrdered() bool {
	return atomic.LoadInt32(&c.requireOrder) == 1
}

func (c *Channel) initPQ() {
	pqSize := int(math.Max(1, float64(c.option.MemQueueSize)/10))

	c.inFlightMutex.Lock()
	c.inFlightMessages = make(map[MessageID]*Message, pqSize)
	c.inFlightPQ = newInFlightPqueue(pqSize)
	atomic.StoreInt64(&c.deferredCount, 0)
	c.inFlightMutex.Unlock()

}

// Exiting returns a boolean indicating if this channel is closed/exiting
func (c *Channel) Exiting() bool {
	return atomic.LoadInt32(&c.exitFlag) == 1
}

// Delete empties the channel and closes
func (c *Channel) Delete() error {
	return c.exit(true)
}

// Close cleanly closes the Channel
func (c *Channel) Close() error {
	return c.exit(false)
}

func (c *Channel) IsWaitingMoreData() bool {
	d, ok := c.backend.(*diskQueueReader)
	if ok {
		return d.IsWaitingMoreData()
	}
	return false
}

func (c *Channel) exit(deleted bool) error {
	c.exitMutex.Lock()
	defer c.exitMutex.Unlock()

	if !atomic.CompareAndSwapInt32(&c.exitFlag, 0, 1) {
		return ErrExiting
	}

	if deleted {
		nsqLog.Logf("CHANNEL(%s): deleting", c.name)

		// since we are explicitly deleting a channel (not just at system exit time)
		// de-register this from the lookupd
		c.notifyCall(c)
	} else {
		nsqLog.Logf("CHANNEL(%s): closing", c.name)
	}

	// this forceably closes clients, client will be removed by client before the
	// client read loop exit.
	c.RLock()
	for _, client := range c.clients {
		client.Exit()
	}
	c.RUnlock()

	close(c.exitChan)
	<-c.exitSyncChan

	if deleted {
		// empty the queue (deletes the backend files, too)
		c.emptyConsume()
		return c.backend.Delete()
	}

	// write anything leftover to disk
	c.flush()
	return c.backend.Close()
}

func (c *Channel) emptyConsume() error {
	c.Lock()
	defer c.Unlock()

	c.initPQ()
	for _, client := range c.clients {
		client.Empty()
	}

	clientMsgChan := c.clientMsgChan
	for {
		select {
		case _, ok := <-clientMsgChan:
			if !ok {
				// c.clientMsgChan may be closed while in this loop
				// so just remove it from the select so we can make progress
				clientMsgChan = nil
			}
		case <-c.requeuedMsgChan:
		default:
			goto finish
		}
	}

finish:
	c.waitingRequeueMutex.Lock()
	for k, _ := range c.waitingRequeueMsgs {
		delete(c.waitingRequeueMsgs, k)
	}
	c.waitingRequeueMutex.Unlock()
	d, ok := c.backend.(*diskQueueReader)
	if ok {
		d.SkipToEnd()
	}
	return nil
}

func (c *Channel) Empty() error {
	if c.IsConsumeDisabled() {
		return ErrConsumeDisabled
	}
	return c.emptyConsume()
}

func (c *Channel) flush() error {
	d, ok := c.backend.(*diskQueueReader)
	if ok {
		d.Flush()
	}
	return nil
}

func (c *Channel) Depth() int64 {
	return c.backend.Depth()
}

func (c *Channel) DepthSize() int64 {
	if d, ok := c.backend.(*diskQueueReader); ok {
		return d.DepthSize()
	}
	return 0
}

func (c *Channel) DepthTimestamp() int64 {
	return atomic.LoadInt64(&c.waitingProcessMsgTs)
}

func (c *Channel) Pause() error {
	return c.doPause(true)
}

func (c *Channel) UnPause() error {
	return c.doPause(false)
}

func (c *Channel) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&c.paused, 1)
	} else {
		atomic.StoreInt32(&c.paused, 0)
	}

	c.RLock()
	for _, client := range c.clients {
		if pause {
			client.Pause()
		} else {
			client.UnPause()
		}
	}
	c.RUnlock()
	return nil
}

func (c *Channel) IsPaused() bool {
	return atomic.LoadInt32(&c.paused) == 1
}

// When topic message is put, update the new end of the queue
func (c *Channel) UpdateQueueEnd(end BackendQueueEnd, forceReload bool) error {
	if end == nil {
		return nil
	}
	changed, err := c.backend.UpdateQueueEnd(end, forceReload)
	if !changed || err != nil {
		return err
	}

	if c.IsConsumeDisabled() {
	} else {
		select {
		case c.endUpdatedChan <- true:
		default:
		}
	}
	return err
}

// TouchMessage resets the timeout for an in-flight message
func (c *Channel) TouchMessage(clientID int64, id MessageID, clientMsgTimeout time.Duration) error {
	c.inFlightMutex.Lock()
	msg, ok := c.inFlightMessages[id]
	if !ok {
		c.inFlightMutex.Unlock()
		nsqLog.Logf("failed while touch: %v, msg not exist", id)
		return ErrMsgNotInFlight
	}
	if msg.clientID != clientID {
		c.inFlightMutex.Unlock()
		return fmt.Errorf("client does not own message : %v vs %v",
			msg.clientID, clientID)
	}
	newTimeout := time.Now().Add(clientMsgTimeout)
	if newTimeout.Sub(msg.deliveryTS) >=
		c.option.MaxMsgTimeout {
		// we would have gone over, set to the max
		newTimeout = msg.deliveryTS.Add(c.option.MaxMsgTimeout)
	}
	msg.pri = newTimeout.UnixNano()
	c.inFlightMutex.Unlock()
	return nil
}

func (c *Channel) ConfirmBackendQueueOnSlave(offset BackendOffset) error {
	// TODO: confirm on slave may exceed the current end, because the buffered write
	// may need to be flushed on slave.
	c.confirmMutex.Lock()
	if len(c.confirmedMsgs) != 0 {
		nsqLog.LogWarningf("should empty confirmed queue on slave.")
	}
	var err error
	if offset < c.GetConfirmedOffset() {
		if nsqLog.Level() > levellogger.LOG_DEBUG {
			nsqLog.LogDebugf("confirm offset less than current: %v, %v", offset, c.GetConfirmedOffset())
		}
	} else {
		_, err = c.backend.SkipReadToOffset(offset)
		if err != nil {
			if !c.Exiting() {
				nsqLog.Logf("confirm read failed: %v, offset: %v", err, offset)
			}
		} else {
			c.UpdateConfirmedOffset(offset)
		}
	}
	c.confirmMutex.Unlock()
	return err
}

// in order not to make the confirm map too large,
// we need handle this case: a old message is not confirmed,
// and we keep all the newer confirmed messages so we can confirm later.
// indicated weather the confirmed offset is changed
func (c *Channel) ConfirmBackendQueue(msg *Message) (BackendOffset, bool) {
	c.confirmMutex.Lock()
	defer c.confirmMutex.Unlock()
	//c.finMsgs[msg.ID] = msg
	curConfirm := c.GetConfirmedOffset()
	if msg.offset < curConfirm {
		nsqLog.LogDebugf("confirmed msg is less than current confirmed offset: %v-%v, %v", msg.ID, msg.offset, curConfirm)
		return curConfirm, false
	}
	c.confirmedMsgs[int64(msg.offset)] = msg
	reduced := false
	for {
		curConfirm = c.GetConfirmedOffset()
		if m, ok := c.confirmedMsgs[int64(curConfirm)]; ok {
			nsqLog.LogDebugf("move confirm: %v to %v, msg: %v",
				curConfirm, curConfirm+m.rawMoveSize, m.ID)
			c.UpdateConfirmedOffset(curConfirm + m.rawMoveSize)
			delete(c.confirmedMsgs, int64(m.offset))
			reduced = true
		} else {
			break
		}
	}
	atomic.StoreInt32(&c.waitingConfirm, int32(len(c.confirmedMsgs)))
	if reduced {
		err := c.backend.ConfirmRead(c.GetConfirmedOffset())
		if err != nil {
			if !c.Exiting() {
				nsqLog.LogErrorf("confirm read failed: %v, msg: %v", err, msg)
			}
			return c.GetConfirmedOffset(), reduced
		}
		if int64(len(c.confirmedMsgs)) < c.option.MaxConfirmWin/2 &&
			atomic.LoadInt32(&c.needNotifyRead) == 1 &&
			!c.IsOrdered() {
			select {
			case c.tryReadBackend <- true:
			default:
			}
		}
	}
	if int64(len(c.confirmedMsgs)) > c.option.MaxConfirmWin {
		curConfirm = c.GetConfirmedOffset()
		c.inFlightMutex.Lock()
		flightCnt := len(c.inFlightMessages)
		c.inFlightMutex.Unlock()
		c.waitingRequeueMutex.Lock()
		reqLen := len(c.waitingRequeueMsgs)
		reqLen += len(c.requeuedMsgChan)
		c.waitingRequeueMutex.Unlock()
		if flightCnt == 0 && reqLen <= 0 {
			nsqLog.LogDebugf("lots of confirmed messages : %v, %v, %v, %v, %v",
				len(c.confirmedMsgs), curConfirm, flightCnt, reqLen)
			//atomic.StoreInt32(&c.needResetReader, 1)
		}
		if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DEBUG {
			nsqLog.LogDebugf("lots of confirmed messages : %v, %v, %v, %v, %v",
				len(c.confirmedMsgs), curConfirm, flightCnt, reqLen)
			//found the message in the flight with offset c.currentLastConfirmed and
			//requeue to client again. This can force the missed message with
			//c.currentLastConfirmed offset
		}
	}
	return c.GetConfirmedOffset(), reduced
	// TODO: if some messages lost while re-queue, it may happen that some messages not
	// in inflight queue and also not wait confirm. In this way, we need reset
	// backend queue to force read the data from disk again.
}

func (c *Channel) IsConfirmed(msg *Message) bool {
	c.confirmMutex.Lock()
	//c.finMsgs[msg.ID] = msg
	_, ok := c.confirmedMsgs[int64(msg.offset)]
	c.confirmMutex.Unlock()
	return ok
}

// FinishMessage successfully discards an in-flight message
func (c *Channel) FinishMessage(clientID int64, id MessageID) (BackendOffset, bool, error) {
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		nsqLog.LogWarningf("message %v fin error: %v from client %v", id, err,
			clientID)
		return 0, false, err
	}
	if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DEBUG {
		nsqMsgTracer.TraceSub(c.GetTopicName(), "FIN", msg.TraceID, msg, strconv.Itoa(int(clientID)))
	}
	if c.e2eProcessingLatencyStream != nil {
		c.e2eProcessingLatencyStream.Insert(msg.Timestamp)
	}
	offset, changed := c.ConfirmBackendQueue(msg)
	return offset, changed, nil
}

func (c *Channel) ContinueConsumeForOrder() {
	if c.IsOrdered() && atomic.LoadInt32(&c.needNotifyRead) == 1 {
		select {
		case c.tryReadBackend <- true:
		default:
		}
	}
}

// RequeueMessage requeues a message based on `time.Duration`, ie:
//
// `timeoutMs` == 0 - requeue a message immediately
// `timeoutMs`  > 0 - asynchronously wait for the specified timeout
//     and requeue a message
//
func (c *Channel) RequeueMessage(clientID int64, id MessageID, timeout time.Duration) error {
	if timeout == 0 {
		// remove from inflight first
		msg, err := c.popInFlightMessage(clientID, id)
		if err != nil {
			nsqLog.LogWarningf("message %v requeue error: %v from client %v", id, err,
				clientID)
			return err
		}
		return c.doRequeue(msg)
	}
	// change the timeout for inflight
	c.inFlightMutex.Lock()
	defer c.inFlightMutex.Unlock()
	msg, ok := c.inFlightMessages[id]
	if !ok {
		nsqLog.Logf("failed requeue for delay: %v, msg not exist", id)
		return ErrMsgNotInFlight
	}
	if msg.clientID != clientID {
		nsqLog.Logf("failed requeue for client not own message: %v: %v vs %v", id, msg.clientID, clientID)
		return fmt.Errorf("client does not own message %v: %v vs %v", id,
			msg.clientID, clientID)
	}
	newTimeout := time.Now().Add(timeout)
	if newTimeout.Sub(msg.deliveryTS) >=
		c.option.MaxReqTimeout {
		// we would have gone over, set to the max
		nsqLog.Logf("requeue message: %v exceed max requeue timeout: %v, %v", id, msg.deliveryTS, newTimeout)
		newTimeout = msg.deliveryTS.Add(c.option.MaxReqTimeout)
	}
	atomic.AddInt64(&c.deferredCount, 1)
	msg.pri = newTimeout.UnixNano()
	msg.isDeferred = true
	return nil
}

func (c *Channel) RequeueClientMessages(clientID int64) {
	if c.Exiting() {
		return
	}
	if c.IsConsumeDisabled() {
		return
	}
	idList := make([]MessageID, 0)
	c.inFlightMutex.Lock()
	for id, msg := range c.inFlightMessages {
		if msg.clientID == clientID {
			idList = append(idList, id)
		}
	}
	c.inFlightMutex.Unlock()
	for _, id := range idList {
		c.RequeueMessage(clientID, id, 0)
	}
	if len(idList) > 0 {
		nsqLog.Logf("client: %v requeued %v messages ",
			clientID, len(idList))
	}
}

func (c *Channel) GetClientsCount() int {
	c.RLock()
	defer c.RUnlock()
	return len(c.clients)
}

func (c *Channel) GetClients() map[int64]Consumer {
	c.RLock()
	defer c.RUnlock()

	results := make(map[int64]Consumer)
	for k, c := range c.clients {
		results[k] = c
	}
	return results
}

// AddClient adds a client to the Channel's client list
func (c *Channel) AddClient(clientID int64, client Consumer) error {
	c.Lock()
	defer c.Unlock()

	if c.IsConsumeDisabled() {
		return ErrConsumeDisabled
	}
	_, ok := c.clients[clientID]
	if ok {
		return nil
	}
	c.clients[clientID] = client
	if len(c.clients) == 1 {
		select {
		case c.tryReadBackend <- true:
		default:
		}
	}
	return nil
}

// RemoveClient removes a client from the Channel's client list
func (c *Channel) RemoveClient(clientID int64) {
	c.Lock()
	defer c.Unlock()

	_, ok := c.clients[clientID]
	if !ok {
		return
	}
	delete(c.clients, clientID)

	if len(c.clients) == 0 && c.ephemeral == true {
		go c.deleter.Do(func() { c.deleteCallback(c) })
	}
}

func (c *Channel) StartInFlightTimeout(msg *Message, clientID int64, timeout time.Duration) error {
	now := time.Now()
	msg.clientID = clientID
	msg.deliveryTS = now
	msg.pri = now.Add(timeout).UnixNano()
	err := c.pushInFlightMessage(msg)
	if err != nil {
		nsqLog.LogWarningf("push message in flight failed: %v, %v", err,
			msg.GetFullMsgID())
		return err
	}
	if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DEBUG {
		nsqMsgTracer.TraceSub(c.GetTopicName(), "START", msg.TraceID, msg, strconv.Itoa(int(clientID)))
	}
	return nil
}

func (c *Channel) GetInflightNum() int {
	c.inFlightMutex.Lock()
	n := len(c.inFlightMessages)
	c.inFlightMutex.Unlock()
	return n
}

func (c *Channel) UpdateConfirmedOffset(offset BackendOffset) {
	atomic.StoreInt64(&c.currentLastConfirmed, int64(offset))
}

func (c *Channel) GetConfirmedOffset() BackendOffset {
	tmp := atomic.LoadInt64(&c.currentLastConfirmed)
	return BackendOffset(tmp)
}

func (c *Channel) GetChannelEnd() BackendOffset {
	return c.backend.GetQueueReadEnd().Offset()
}

// doRequeue performs the low level operations to requeue a message
func (c *Channel) doRequeue(m *Message) error {
	if c.Exiting() {
		return ErrExiting
	}
	if c.IsConsumeDisabled() {
		return ErrConsumeDisabled
	}
	select {
	case <-c.exitChan:
		nsqLog.LogDebugf("requeue message failed for existing: %v ", m.ID)
		return ErrExiting
	case c.requeuedMsgChan <- m:
	default:
		c.waitingRequeueMutex.Lock()
		c.waitingRequeueMsgs[m.ID] = m
		c.waitingRequeueMutex.Unlock()
	}
	atomic.AddUint64(&c.requeueCount, 1)
	if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DEBUG {
		nsqMsgTracer.TraceSub(c.GetTopicName(), "REQ", m.TraceID, m, strconv.Itoa(int(m.clientID)))
	}
	return nil
}

// pushInFlightMessage atomically adds a message to the in-flight dictionary
func (c *Channel) pushInFlightMessage(msg *Message) error {
	c.inFlightMutex.Lock()
	if c.IsConsumeDisabled() {
		return ErrConsumeDisabled
	}
	_, ok := c.inFlightMessages[msg.ID]
	if ok {
		c.inFlightMutex.Unlock()
		return ErrMsgAlreadyInFlight
	}
	c.inFlightMessages[msg.ID] = msg
	c.inFlightPQ.Push(msg)
	c.inFlightMutex.Unlock()
	return nil
}

// popInFlightMessage atomically removes a message from the in-flight dictionary
func (c *Channel) popInFlightMessage(clientID int64, id MessageID) (*Message, error) {
	c.inFlightMutex.Lock()
	msg, ok := c.inFlightMessages[id]
	if !ok {
		c.inFlightMutex.Unlock()
		return nil, ErrMsgNotInFlight
	}
	if msg.clientID != clientID {
		c.inFlightMutex.Unlock()
		return nil, fmt.Errorf("client does not own message : %v vs %v",
			msg.clientID, clientID)
	}
	if msg.isDeferred {
		nsqLog.LogWarningf("should never pop a deferred message here unless the timeout : %v", msg.ID)
		c.inFlightMutex.Unlock()
		return nil, ErrMsgDeferred
	}
	delete(c.inFlightMessages, id)
	if msg.index != -1 {
		c.inFlightPQ.Remove(msg.index)
	}
	c.inFlightMutex.Unlock()
	return msg, nil
}

func (c *Channel) IsConsumeDisabled() bool {
	return atomic.LoadInt32(&c.consumeDisabled) == 1
}

func (c *Channel) DisableConsume(disable bool) {
	c.Lock()
	defer c.Unlock()
	if disable {
		if !atomic.CompareAndSwapInt32(&c.consumeDisabled, 0, 1) {
			return
		}
		nsqLog.Logf("channel %v disabled for consume", c.name)
		for cid, client := range c.clients {
			client.Exit()
			delete(c.clients, cid)
		}
		c.initPQ()
		c.drainChannelWaiting(true)
	} else {
		nsqLog.Logf("channel %v enabled for consume", c.name)
		// we need reset backend read position to confirm position
		// since we dropped all inflight and requeue data while disable consume.
		done := false
		for !done {
			select {
			case m := <-c.clientMsgChan:
				nsqLog.Logf("ignored a read message %v at offset %v while enable consume", m.ID, m.offset)
			case <-c.requeuedMsgChan:
			default:
				done = true
			}
		}
		atomic.StoreInt32(&c.consumeDisabled, 0)
		select {
		case c.readerChanged <- BackendOffset(-1):
		default:
		}
	}
	c.notifyCall(c)
}

func (c *Channel) drainChannelWaiting(clearConfirmed bool) error {
	c.waitingRequeueMutex.Lock()
	for k, _ := range c.waitingRequeueMsgs {
		delete(c.waitingRequeueMsgs, k)
	}
	c.waitingRequeueMutex.Unlock()
	if clearConfirmed {
		c.confirmMutex.Lock()
		c.confirmedMsgs = make(map[int64]*Message)
		atomic.StoreInt32(&c.waitingConfirm, 0)
		c.confirmMutex.Unlock()
	}
	atomic.StoreInt64(&c.waitingProcessMsgTs, 0)

	done := false
	for !done {
		select {
		case m := <-c.clientMsgChan:
			nsqLog.Logf("ignored a read message %v at offset %v while drain channel", m.ID, m.offset)
		case <-c.requeuedMsgChan:
		default:
			done = true
		}
	}
	return nil
}

func (c *Channel) TryWakeupRead() {
	if c.IsConsumeDisabled() {
		return
	}
	if c.IsOrdered() {
		return
	}
	select {
	case c.tryReadBackend <- true:
	default:
	}
	if nsqLog.Level() >= levellogger.LOG_DEBUG {
		nsqLog.LogDebugf("channel consume try wakeup : %v", c.name)
	}
}

func (c *Channel) resetReaderToConfirmed() error {
	atomic.StoreInt64(&c.waitingProcessMsgTs, 0)
	atomic.CompareAndSwapInt32(&c.needResetReader, 1, 0)
	confirmed, err := c.backend.ResetReadToConfirmed()
	if err != nil {
		nsqLog.LogWarningf("reset read to confirmed error: %v", err)
		return err
	}
	nsqLog.Logf("reset channel %v reader to confirm: %v", c.name, confirmed)
	return nil
}

// messagePump reads messages from either memory or backend and sends
// messages to clients over a go chan
func (c *Channel) messagePump() {
	var msg *Message
	var data ReadResult
	var err error
	var lastMsg Message
	var lastDataResult ReadResult
	isSkipped := false
	origReadChan := make(chan ReadResult, 1)
	var readChan <-chan ReadResult
	var waitEndUpdated chan bool

	maxWin := int32(c.option.MaxConfirmWin)
	resumedFirst := true
	d := c.backend
	needReadBackend := true
	lastDataNeedRead := false
	readBackendWait := false

LOOP:
	for {
		// do an extra check for closed exit before we select on all the memory/backend/exitChan
		// this solves the case where we are closed and something else is draining clientMsgChan into
		// backend. we don't want to reverse that
		if atomic.LoadInt32(&c.exitFlag) == 1 {
			goto exit
		}

		if atomic.CompareAndSwapInt32(&c.needResetReader, 1, 0) {
			nsqLog.Warningf("reset the reader : %v", c.currentLastConfirmed)
			err = c.resetReaderToConfirmed()
			// if reset failed, we should not drain the waiting data
			if err == nil {
				c.drainChannelWaiting(false)
				lastDataNeedRead = false
				// since the reader is reset, we should drain the previous data.
				select {
				case <-origReadChan:
				default:
				}
				lastMsg = Message{}
			}
			readChan = origReadChan
			needReadBackend = true
			readBackendWait = false
		} else if readBackendWait {
			readChan = nil
			needReadBackend = false
		} else if atomic.LoadInt32(&c.waitingConfirm) > maxWin {
			if nsqLog.Level() >= levellogger.LOG_DEBUG {
				nsqLog.LogDebugf("channel reader is holding: %v, %v, %v",
					atomic.LoadInt32(&c.waitingConfirm),
					c.currentLastConfirmed,
					c.name)
			}
			atomic.StoreInt32(&c.needNotifyRead, 1)
			readChan = nil
			needReadBackend = false
		} else {
			readChan = origReadChan
			needReadBackend = true
		}

		if c.IsConsumeDisabled() {
			readChan = nil
			needReadBackend = false
			nsqLog.Logf("channel consume is disabled : %v", c.name)
			if lastMsg.ID > 0 {
				nsqLog.Logf("consume disabled at last read message: %v:%v", lastMsg.ID, lastMsg.offset)
				lastMsg = Message{}
			}
		}

		if needReadBackend {
			if !lastDataNeedRead {
				dataRead, hasData := d.TryReadOne()
				if hasData {
					lastDataNeedRead = true
					origReadChan <- dataRead
					readChan = origReadChan
					waitEndUpdated = nil
				} else {
					if nsqLog.Level() >= levellogger.LOG_DEBUG {
						nsqLog.LogDebugf("no data to be read: %v", c.name)
					}
					readChan = nil
					waitEndUpdated = c.endUpdatedChan
					atomic.StoreInt64(&c.waitingProcessMsgTs, 0)
				}
			} else {
				readChan = origReadChan
				waitEndUpdated = nil
			}
		} else {
			waitEndUpdated = nil
		}

		select {
		case <-c.exitChan:
			goto exit
		case msg = <-c.requeuedMsgChan:
			if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DETAIL {
				nsqLog.LogDebugf("read message %v from requeue", msg.ID)
				nsqMsgTracer.TraceSub(c.GetTopicName(), "READ_REQ", msg.TraceID, msg, "0")
			}
		case data = <-readChan:
			lastDataNeedRead = false
			if data.Err != nil {
				nsqLog.LogErrorf("failed to read message - %s", data.Err)
				// TODO: fix corrupt file from other replica.
				// and should handle the confirm offset, since some skipped data
				// may never be confirmed any more
				c.backend.(*diskQueueReader).SkipToNext()
				isSkipped = true
				time.Sleep(time.Millisecond * 100)
				continue LOOP
			}
			msg, err = decodeMessage(data.Data)
			if err != nil {
				nsqLog.LogErrorf("failed to decode message - %s - %v", err, data)
				continue LOOP
			}
			msg.offset = data.Offset
			msg.rawMoveSize = data.MovedSize
			if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_DETAIL {
				nsqMsgTracer.TraceSub(c.GetTopicName(), "READ_QUEUE", msg.TraceID, msg, "0")
			}

			if lastMsg.ID > 0 && msg.ID < lastMsg.ID {
				// note: this may happen if the reader pefetch some data not committed by the disk writer
				// we need read it again later.
				nsqLog.Warningf("read a message with less message ID: %v vs %v, raw data: %v", msg.ID, lastMsg.ID, data)
				nsqLog.Warningf("last raw data: %v", lastDataResult)
				time.Sleep(time.Millisecond * 5)
				if diskQ, ok := c.backend.(*diskQueueReader); ok {
					diskQ.resetLastReadOne(data.Offset, int32(data.MovedSize))
				}
				lastMsg = *msg
				lastDataResult = data
				continue LOOP
			}

			atomic.StoreInt64(&c.waitingProcessMsgTs, msg.Timestamp)
			lastDataResult = data
			if isSkipped {
				// TODO: store the skipped info to retry error if possible.
				nsqLog.LogWarningf("skipped message from %v:%v to the : %v:%v", lastMsg.ID, lastMsg.offset, msg.ID, msg.offset)
			}
			if resumedFirst {
				if nsqLog.Level() >= levellogger.LOG_DEBUG {
					nsqLog.LogDebugf("resumed first messsage %v at offset: %v", msg.ID, msg.offset)
				}
				resumedFirst = false
			}
			isSkipped = false
		case <-c.tryReadBackend:
			atomic.StoreInt32(&c.needNotifyRead, 0)
			readBackendWait = false
			resumedFirst = true
			continue LOOP
		case resetOffset := <-c.readerChanged:
			nsqLog.Infof("got reader reset notify:%v ", resetOffset)
			if resetOffset == BackendOffset(-1) {
				atomic.StoreInt32(&c.needResetReader, 1)
			} else {
				d, ok := c.backend.(*diskQueueReader)
				if ok {
					_, err = d.ResetReadToOffset(resetOffset)
					if err != nil {
						nsqLog.Warningf("failed to reset reader to %v, %v", resetOffset, err)
					} else {
						c.drainChannelWaiting(true)
						lastDataNeedRead = false
						// since the reader is reset, we should drain the previous data.
						select {
						case <-origReadChan:
						default:
						}
						lastMsg = Message{}
					}
				} else {
					nsqLog.Warningf("failed to reset reader to %v: not a disk queue backend", resetOffset)
				}
				needReadBackend = true
				readBackendWait = false
			}
			continue LOOP
		case <-waitEndUpdated:
			continue LOOP
		}

		if msg == nil {
			continue
		}

		lastMsg = *msg

		if c.IsConsumeDisabled() {
			continue
		}
		if c.IsOrdered() {
			if msg.offset != c.GetConfirmedOffset() {
				nsqLog.Infof("read a message not in ordered: %v, %v", msg.offset, c.GetConfirmedOffset())
				atomic.StoreInt32(&c.needResetReader, 1)
				continue
			}
		}
		msg.Attempts++
		msg.isDeferred = false
		if c.IsOrdered() {
			atomic.StoreInt32(&c.needNotifyRead, 1)
			readBackendWait = true
		}
		select {
		case c.clientMsgChan <- msg:
		case resetOffset := <-c.readerChanged:
			nsqLog.Infof("got reader reset notify while dispatch message:%v ", resetOffset)
			if resetOffset == BackendOffset(-1) {
				atomic.StoreInt32(&c.needResetReader, 1)
			} else {
				d, ok := c.backend.(*diskQueueReader)
				if ok {
					_, err = d.ResetReadToOffset(resetOffset)
					if err != nil {
						nsqLog.Warningf("failed to reset reader to %v, %v", resetOffset, err)
					} else {
						c.drainChannelWaiting(true)
						lastDataNeedRead = false
						// since the reader is reset, we should drain the previous data.
						select {
						case <-origReadChan:
						default:
						}
						lastMsg = Message{}
					}
				} else {
					nsqLog.Warningf("failed to reset reader to %v: not a disk queue backend", resetOffset)
				}
				needReadBackend = true
				readBackendWait = false
			}
		case <-c.exitChan:
			goto exit
		}
		msg = nil
		// the client will call back to mark as in-flight w/ its info
	}

exit:
	nsqLog.Logf("CHANNEL(%s): closing ... messagePump", c.name)
	close(c.clientMsgChan)
	close(c.exitSyncChan)
}

func (c *Channel) GetChannelDebugStats() string {
	debugStr := ""
	c.inFlightMutex.Lock()
	inFlightCount := len(c.inFlightMessages)
	debugStr += fmt.Sprintf("inflight %v messages : ", inFlightCount)
	for _, msg := range c.inFlightMessages {
		debugStr += fmt.Sprintf("%v(%v),", msg.ID, msg.offset)
	}
	c.inFlightMutex.Unlock()
	debugStr += "\n"
	c.confirmMutex.Lock()
	debugStr += fmt.Sprintf("channel end : %v, current confirm %v, confirmed %v messages: ",
		c.GetChannelEnd(),
		c.GetConfirmedOffset(), len(c.confirmedMsgs))
	for _, msg := range c.confirmedMsgs {
		debugStr += fmt.Sprintf("%v(%v), ", msg.ID, msg.offset)
	}
	c.confirmMutex.Unlock()
	debugStr += "\n"
	return debugStr
}

func (c *Channel) processInFlightQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	flightCnt := 0
	requeuedCnt := 0
	for {
		if c.IsConsumeDisabled() {
			goto exit
		}
		c.inFlightMutex.Lock()
		msg, _ := c.inFlightPQ.PeekAndShift(t)
		flightCnt = len(c.inFlightMessages)
		if msg == nil {
			if atomic.LoadInt32(&c.waitingConfirm) > 1 || flightCnt > 1 {
				nsqLog.LogDebugf("no timeout, inflight %v, waiting confirm: %v, confirmed: %v",
					flightCnt, atomic.LoadInt32(&c.waitingConfirm),
					c.GetConfirmedOffset())
			}
			c.inFlightMutex.Unlock()
			goto exit
		}
		dirty = true

		_, ok := c.inFlightMessages[msg.ID]
		if !ok {
			c.inFlightMutex.Unlock()
			goto exit
		}
		delete(c.inFlightMessages, msg.ID)
		// note: if this message is deferred by client, we treat it as a delay message,
		// so we consider it is by demanded to delay not timeout of message.
		if msg.isDeferred {
			atomic.AddInt64(&c.deferredCount, -1)
		} else {
			atomic.AddUint64(&c.timeoutCount, 1)
		}
		c.inFlightMutex.Unlock()

		c.RLock()
		client, ok := c.clients[msg.clientID]
		c.RUnlock()
		requeuedCnt++
		c.doRequeue(msg)
		if ok {
			client.TimedOutMessage(msg.isDeferred)
		}
		if c.IsTraced() || nsqLog.Level() >= levellogger.LOG_INFO {
			nsqMsgTracer.TraceSub(c.GetTopicName(), "TIMEOUT", msg.TraceID, msg, strconv.Itoa(int(msg.clientID)))
		}
	}

exit:
	// try requeue the messages that waiting.
	stopScan := false
	c.waitingRequeueMutex.Lock()
	reqLen := len(c.requeuedMsgChan) + len(c.waitingRequeueMsgs)
	if !c.IsConsumeDisabled() {
		if len(c.waitingRequeueMsgs) > 1 {
			nsqLog.LogDebugf("requeue waiting messages: %v", len(c.waitingRequeueMsgs))
		}

		for k, m := range c.waitingRequeueMsgs {
			select {
			case c.requeuedMsgChan <- m:
				delete(c.waitingRequeueMsgs, k)
				requeuedCnt++
			default:
				stopScan = true
			}
			if stopScan {
				break
			}
		}
		reqLen += len(c.requeuedMsgChan)
	}
	c.waitingRequeueMutex.Unlock()
	if (atomic.LoadInt32(&c.waitingConfirm) >=
		int32(c.option.MaxConfirmWin)) &&
		(flightCnt == 0) && (reqLen == 0) &&
		(requeuedCnt <= 0) && (!dirty) {
		diff := time.Now().Unix() - atomic.LoadInt64(&c.processResetReaderTime)
		if diff > resetReaderTimeoutSec && atomic.LoadInt64(&c.processResetReaderTime) > 0 {
			nsqLog.Logf("try reset reader since no inflight and requeued for too long (%v): %v, %v",
				diff,
				atomic.LoadInt32(&c.waitingConfirm), requeuedCnt)

			atomic.StoreInt32(&c.needResetReader, 1)
			select {
			case c.readerChanged <- BackendOffset(-1):
			default:
			}
		}
	} else {
		atomic.StoreInt64(&c.processResetReaderTime, time.Now().Unix())
	}

	return dirty
}
