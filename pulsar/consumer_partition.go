// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"container/list"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/pulsar-client-go/pulsar/backoff"

	"google.golang.org/protobuf/proto"

	"github.com/apache/pulsar-client-go/pulsar/crypto"
	"github.com/apache/pulsar-client-go/pulsar/internal"
	"github.com/apache/pulsar-client-go/pulsar/internal/compression"
	cryptointernal "github.com/apache/pulsar-client-go/pulsar/internal/crypto"
	pb "github.com/apache/pulsar-client-go/pulsar/internal/pulsar_proto"
	"github.com/apache/pulsar-client-go/pulsar/log"
	"github.com/bits-and-blooms/bitset"
	"github.com/pkg/errors"

	uAtomic "go.uber.org/atomic"
)

type consumerState int

const (
	// consumer states
	consumerInit = iota
	consumerReady
	consumerClosing
	consumerClosed
)

var (
	ErrInvalidAck = errors.New("invalid ack")
)

func (s consumerState) String() string {
	switch s {
	case consumerInit:
		return "Initializing"
	case consumerReady:
		return "Ready"
	case consumerClosing:
		return "Closing"
	case consumerClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

type SubscriptionMode int

const (
	// Make the subscription to be backed by a durable cursor that will retain messages and persist the current
	// position
	Durable SubscriptionMode = iota

	// Lightweight subscription mode that doesn't have a durable cursor associated
	NonDurable
)

const (
	initialReceiverQueueSize           = 1
	receiverQueueExpansionMemThreshold = 0.75
)

const (
	noMessageEntry = -1
)

type partitionConsumerOpts struct {
	topic                       string
	consumerName                string
	subscription                string
	subscriptionType            SubscriptionType
	subscriptionInitPos         SubscriptionInitialPosition
	partitionIdx                int
	receiverQueueSize           int
	autoReceiverQueueSize       bool
	nackRedeliveryDelay         time.Duration
	nackBackoffPolicy           NackBackoffPolicy
	metadata                    map[string]string
	subProperties               map[string]string
	replicateSubscriptionState  bool
	startMessageID              *trackingMessageID
	startMessageIDInclusive     bool
	subscriptionMode            SubscriptionMode
	readCompacted               bool
	disableForceTopicCreation   bool
	interceptors                ConsumerInterceptors
	maxReconnectToBroker        *uint
	backOffPolicyFunc           func() backoff.Policy
	keySharedPolicy             *KeySharedPolicy
	schema                      Schema
	decryption                  *MessageDecryptionInfo
	ackWithResponse             bool
	maxPendingChunkedMessage    int
	expireTimeOfIncompleteChunk time.Duration
	autoAckIncompleteChunk      bool
	// in failover mode, this callback will be called when consumer change
	consumerEventListener ConsumerEventListener
	enableBatchIndexAck   bool
	ackGroupingOptions    *AckGroupingOptions
}

type ConsumerEventListener interface {
	BecameActive(consumer Consumer, topicName string, partition int32)
	BecameInactive(consumer Consumer, topicName string, partition int32)
}

type partitionConsumer struct {
	client *client

	// this is needed for sending ConsumerMessage on the messageCh
	parentConsumer Consumer
	state          uAtomic.Int32
	options        *partitionConsumerOpts

	conn         atomic.Pointer[internal.Connection]
	cnxKeySuffix int32

	topic        string
	name         string
	consumerID   uint64
	partitionIdx int32

	// shared channel
	messageCh chan ConsumerMessage

	// the number of message slots available
	availablePermits *availablePermits

	// the size of the queue channel for buffering messages
	maxQueueSize    int32
	queueCh         chan []*message
	startMessageID  atomicMessageID
	lastDequeuedMsg *trackingMessageID

	// This is used to track the seeking message id during the seek operation.
	// It will be set to nil after seek completes and reconnected.
	seekMessageID atomicMessageID

	currentQueueSize       uAtomic.Int32
	scaleReceiverQueueHint uAtomic.Bool
	incomingMessages       uAtomic.Int32

	eventsCh        chan interface{}
	connectedCh     chan struct{}
	connectClosedCh chan *connectionClosed
	closeCh         chan struct{}
	clearQueueCh    chan func(id *trackingMessageID)

	nackTracker *negativeAcksTracker
	dlq         *dlqRouter

	log                  log.Logger
	compressionProviders sync.Map //map[pb.CompressionType]compression.Provider
	metrics              *internal.LeveledMetrics
	decryptor            cryptointernal.Decryptor
	schemaInfoCache      *schemaInfoCache

	chunkedMsgCtxMap   *chunkedMsgCtxMap
	unAckChunksTracker *unAckChunksTracker
	ackGroupingTracker ackGroupingTracker

	lastMessageInBroker *trackingMessageID

	redirectedClusterURI string
	backoffPolicyFunc    func() backoff.Policy

	dispatcherSeekingControlCh chan struct{}
	// handle to the dispatcher goroutine
	isSeeking atomic.Bool
	// After executing seekByTime, the client is unaware of the startMessageId.
	// Use this flag to compare markDeletePosition with BrokerLastMessageId when checking hasMoreMessages.
	hasSoughtByTime atomic.Bool
	ctx             context.Context
	cancelFunc      context.CancelFunc
}

// pauseDispatchMessage used to discard the message in the dispatcher goroutine.
// This method will be called When the parent consumer performs the seek operation.
// After the seek operation, the dispatcher will continue dispatching messages automatically.
func (pc *partitionConsumer) pauseDispatchMessage() {
	pc.dispatcherSeekingControlCh <- struct{}{}
}

func (pc *partitionConsumer) Topic() string {
	return pc.topic
}

func (pc *partitionConsumer) ActiveConsumerChanged(isActive bool) {
	listener := pc.options.consumerEventListener
	if listener == nil {
		// didn't set a listener
		return
	}
	if isActive {
		listener.BecameActive(pc.parentConsumer, pc.topic, pc.partitionIdx)
	} else {
		listener.BecameInactive(pc.parentConsumer, pc.topic, pc.partitionIdx)
	}
}

type availablePermits struct {
	permits uAtomic.Int32
	pc      *partitionConsumer
}

func (p *availablePermits) inc() {
	// atomic add availablePermits
	p.add(1)
}

func (p *availablePermits) add(delta int32) {
	p.permits.Add(delta)
	p.flowIfNeed()
}

func (p *availablePermits) reset() {
	p.permits.Store(0)
}

func (p *availablePermits) get() int32 {
	return p.permits.Load()
}

func (p *availablePermits) flowIfNeed() {
	// TODO implement a better flow controller
	// send more permits if needed
	var flowThreshold int32
	if p.pc.options.autoReceiverQueueSize {
		flowThreshold = int32(math.Max(float64(p.pc.currentQueueSize.Load()/2), 1))
	} else {
		flowThreshold = int32(math.Max(float64(p.pc.maxQueueSize/2), 1))
	}

	current := p.get()
	if current >= flowThreshold {
		availablePermits := current
		requestedPermits := current
		// check if permits changed
		if !p.permits.CompareAndSwap(current, 0) {
			return
		}

		p.pc.log.Debugf("requesting more permits=%d available=%d", requestedPermits, availablePermits)
		if err := p.pc.internalFlow(uint32(requestedPermits)); err != nil {
			p.pc.log.WithError(err).Error("unable to send permits")
		}
	}
}

// atomicMessageID is a wrapper for trackingMessageID to make get and set atomic
type atomicMessageID struct {
	msgID *trackingMessageID
	sync.RWMutex
}

func (a *atomicMessageID) get() *trackingMessageID {
	a.RLock()
	defer a.RUnlock()
	return a.msgID
}

func (a *atomicMessageID) set(msgID *trackingMessageID) {
	a.Lock()
	defer a.Unlock()
	a.msgID = msgID
}

type schemaInfoCache struct {
	lock   sync.RWMutex
	cache  map[string]Schema
	client *client
	topic  string
}

func newSchemaInfoCache(client *client, topic string) *schemaInfoCache {
	return &schemaInfoCache{
		cache:  make(map[string]Schema),
		client: client,
		topic:  topic,
	}
}

func (s *schemaInfoCache) Get(schemaVersion []byte) (schema Schema, err error) {
	key := hex.EncodeToString(schemaVersion)
	s.lock.RLock()
	schema, ok := s.cache[key]
	s.lock.RUnlock()
	if ok {
		return schema, nil
	}

	//	cache missed, try to use lookupService to find schema info
	lookupSchema, err := s.client.lookupService.GetSchema(s.topic, schemaVersion)
	if err != nil {
		return nil, err
	}
	schema, err = NewSchema(
		//	lookupSchema.SchemaType is internal package SchemaType type,
		//	we need to cast it to pulsar.SchemaType as soon as we use it in current pulsar package
		SchemaType(lookupSchema.SchemaType),
		lookupSchema.Data,
		lookupSchema.Properties,
	)
	if err != nil {
		return nil, err
	}

	s.add(key, schema)
	return schema, nil
}

func (s *schemaInfoCache) add(schemaVersionHash string, schema Schema) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.cache[schemaVersionHash] = schema
}

func newPartitionConsumer(parent Consumer, client *client, options *partitionConsumerOpts,
	messageCh chan ConsumerMessage, dlq *dlqRouter,
	metrics *internal.LeveledMetrics) (*partitionConsumer, error) {
	var boFunc func() backoff.Policy
	if options.backOffPolicyFunc != nil {
		boFunc = options.backOffPolicyFunc
	} else {
		boFunc = backoff.NewDefaultBackoff
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	pc := &partitionConsumer{
		parentConsumer:             parent,
		client:                     client,
		options:                    options,
		cnxKeySuffix:               client.cnxPool.GenerateRoundRobinIndex(),
		topic:                      options.topic,
		name:                       options.consumerName,
		consumerID:                 client.rpcClient.NewConsumerID(),
		partitionIdx:               int32(options.partitionIdx),
		eventsCh:                   make(chan interface{}, 10),
		maxQueueSize:               int32(options.receiverQueueSize),
		queueCh:                    make(chan []*message, options.receiverQueueSize),
		startMessageID:             atomicMessageID{msgID: options.startMessageID},
		seekMessageID:              atomicMessageID{msgID: nil},
		connectedCh:                make(chan struct{}),
		messageCh:                  messageCh,
		connectClosedCh:            make(chan *connectionClosed, 1),
		closeCh:                    make(chan struct{}),
		clearQueueCh:               make(chan func(id *trackingMessageID)),
		compressionProviders:       sync.Map{},
		dlq:                        dlq,
		metrics:                    metrics,
		schemaInfoCache:            newSchemaInfoCache(client, options.topic),
		backoffPolicyFunc:          boFunc,
		dispatcherSeekingControlCh: make(chan struct{}),
		ctx:                        ctx,
		cancelFunc:                 cancelFunc,
	}
	if pc.options.autoReceiverQueueSize {
		pc.currentQueueSize.Store(initialReceiverQueueSize)
		pc.client.memLimit.RegisterTrigger(pc.shrinkReceiverQueueSize)
	} else {
		pc.currentQueueSize.Store(int32(pc.options.receiverQueueSize))
	}
	pc.availablePermits = &availablePermits{pc: pc}
	pc.chunkedMsgCtxMap = newChunkedMsgCtxMap(options.maxPendingChunkedMessage, pc)
	pc.unAckChunksTracker = newUnAckChunksTracker(pc)
	pc.ackGroupingTracker = newAckGroupingTracker(options.ackGroupingOptions,
		func(id MessageID) { pc.sendIndividualAck(id) },
		func(id MessageID) { pc.sendCumulativeAck(id) },
		func(ids []*pb.MessageIdData) {
			pc.eventsCh <- &ackListRequest{
				errCh:  nil, // ignore the error
				msgIDs: ids,
			}
		})
	pc.setConsumerState(consumerInit)
	pc.log = client.log.SubLogger(log.Fields{
		"name":         pc.name,
		"topic":        options.topic,
		"subscription": options.subscription,
		"consumerID":   pc.consumerID,
	})

	var decryptor cryptointernal.Decryptor
	if pc.options.decryption == nil {
		decryptor = cryptointernal.NewNoopDecryptor() // default to noopDecryptor
	} else {
		decryptor = cryptointernal.NewConsumerDecryptor(
			options.decryption.KeyReader,
			options.decryption.MessageCrypto,
			pc.log,
		)
	}

	pc.decryptor = decryptor

	pc.nackTracker = newNegativeAcksTracker(pc, options.nackRedeliveryDelay, options.nackBackoffPolicy, pc.log)

	err := pc.grabConn("")
	if err != nil {
		pc.log.WithError(err).Error("Failed to create consumer")
		pc.nackTracker.Close()
		pc.ackGroupingTracker.close()
		pc.chunkedMsgCtxMap.Close()
		return nil, err
	}
	pc.log.Info("Created consumer")
	pc.setConsumerState(consumerReady)

	startingMessageID := pc.startMessageID.get()
	if pc.options.startMessageIDInclusive && startingMessageID != nil && startingMessageID.equal(latestMessageID) {
		msgIDResp, err := pc.requestGetLastMessageID()
		if err != nil {
			pc.Close()
			return nil, err
		}
		msgID := convertToMessageID(msgIDResp.GetLastMessageId())
		if msgID.entryID != noMessageEntry {
			pc.startMessageID.set(msgID)

			// use the WithoutClear version because the dispatcher is not started yet
			err = pc.requestSeekWithoutClear(msgID.messageID)
			if err != nil {
				pc.Close()
				return nil, err
			}
		}
	}

	go pc.dispatcher()

	go pc.runEventsLoop()

	return pc, nil
}

func (pc *partitionConsumer) unsubscribe(force bool) error {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to unsubscribe closing or closed consumer")
		return errors.New("consumer state is closed")
	}

	req := &unsubscribeRequest{doneCh: make(chan struct{}), force: force}
	pc.eventsCh <- req

	// wait for the request to complete
	<-req.doneCh
	return req.err
}

// ackIDCommon handles common logic for acknowledging messages with or without transactions.
// withTxn should be set to true when dealing with transactions.
func (pc *partitionConsumer) ackIDCommon(msgID MessageID, withResponse bool, txn Transaction) error {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to ack by closing or closed consumer")
		return errors.New("consumer state is closed")
	}

	if cmid, ok := msgID.(*chunkMessageID); ok {
		if txn == nil {
			return pc.unAckChunksTracker.ack(cmid)
		}
		return pc.unAckChunksTracker.ackWithTxn(cmid, txn)
	}

	trackingID := toTrackingMessageID(msgID)

	if trackingID != nil && trackingID.ack() {
		// All messages in the same batch have been acknowledged, we only need to acknowledge the
		// MessageID that represents the entry that stores the whole batch
		trackingID = &trackingMessageID{
			messageID: &messageID{
				ledgerID: trackingID.ledgerID,
				entryID:  trackingID.entryID,
			},
		}
		pc.metrics.AcksCounter.Inc()
		pc.metrics.ProcessingTime.Observe(float64(time.Now().UnixNano()-trackingID.receivedTime.UnixNano()) / 1.0e9)
	} else if !pc.options.enableBatchIndexAck {
		return nil
	}

	var err error
	if withResponse {
		if txn != nil {
			ackReq := pc.sendIndividualAckWithTxn(trackingID, txn.(*transaction))
			<-ackReq.doneCh
			err = ackReq.err
		} else {
			ackReq := pc.sendIndividualAck(trackingID)
			<-ackReq.doneCh
			err = ackReq.err
		}
	} else {
		pc.ackGroupingTracker.add(trackingID)
	}
	pc.options.interceptors.OnAcknowledge(pc.parentConsumer, msgID)
	return err
}

// AckIDWithTxn acknowledges the consumption of a message with transaction.
func (pc *partitionConsumer) AckIDWithTxn(msgID MessageID, txn Transaction) error {
	return pc.ackIDCommon(msgID, true, txn)
}

// ackID acknowledges the consumption of a message and optionally waits for response from the broker.
func (pc *partitionConsumer) ackID(msgID MessageID, withResponse bool) error {
	return pc.ackIDCommon(msgID, withResponse, nil)
}

func (pc *partitionConsumer) internalAckWithTxn(req *ackWithTxnRequest) {
	defer close(req.doneCh)
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to ack by closing or closed consumer")
		req.err = newError(ConsumerClosed, "Failed to ack by closing or closed consumer")
		return
	}
	if req.Transaction.state.Load() != int32(TxnOpen) {
		pc.log.WithField("state", req.Transaction.state.Load()).Error("Failed to ack by a non-open transaction.")
		req.err = newError(InvalidStatus, "Failed to ack by a non-open transaction.")
		return
	}
	msgID := req.msgID

	messageIDs := make([]*pb.MessageIdData, 1)
	messageIDs[0] = &pb.MessageIdData{
		LedgerId: proto.Uint64(uint64(msgID.ledgerID)),
		EntryId:  proto.Uint64(uint64(msgID.entryID)),
	}
	if pc.options.enableBatchIndexAck && msgID.tracker != nil {
		ackSet := msgID.tracker.toAckSet()
		if ackSet != nil {
			messageIDs[0].AckSet = ackSet
		}
	}

	reqID := pc.client.rpcClient.NewRequestID()
	txnID := req.Transaction.GetTxnID()
	cmdAck := &pb.CommandAck{
		ConsumerId:     proto.Uint64(pc.consumerID),
		MessageId:      messageIDs,
		AckType:        pb.CommandAck_Individual.Enum(),
		TxnidMostBits:  proto.Uint64(txnID.MostSigBits),
		TxnidLeastBits: proto.Uint64(txnID.LeastSigBits),
	}

	if err := req.Transaction.registerAckTopic(pc.options.topic, pc.options.subscription); err != nil {
		req.err = err
		return
	}

	if err := req.Transaction.registerSendOrAckOp(); err != nil {
		req.err = err
		return
	}

	cmdAck.RequestId = proto.Uint64(reqID)
	_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), reqID, pb.BaseCommand_ACK, cmdAck)
	if err != nil {
		pc.log.WithError(err).Error("Ack with response error")
	}
	req.Transaction.endSendOrAckOp(err)
	req.err = err
}

func (pc *partitionConsumer) internalUnsubscribe(unsub *unsubscribeRequest) {
	defer close(unsub.doneCh)

	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to unsubscribe closing or closed consumer")
		return
	}

	pc.setConsumerState(consumerClosing)
	requestID := pc.client.rpcClient.NewRequestID()
	cmdUnsubscribe := &pb.CommandUnsubscribe{
		RequestId:  proto.Uint64(requestID),
		ConsumerId: proto.Uint64(pc.consumerID),
		Force:      proto.Bool(unsub.force),
	}
	_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), requestID, pb.BaseCommand_UNSUBSCRIBE, cmdUnsubscribe)
	if err != nil {
		pc.log.WithError(err).Error("Failed to unsubscribe consumer")
		unsub.err = err
		// Set the state to ready for closing the consumer
		pc.setConsumerState(consumerReady)
		// Should'nt remove the consumer handler
		return
	}

	pc._getConn().DeleteConsumeHandler(pc.consumerID)
	if pc.nackTracker != nil {
		pc.nackTracker.Close()
	}
	pc.log.Infof("The consumer[%d] successfully unsubscribed", pc.consumerID)
	pc.setConsumerState(consumerClosed)
}

func (pc *partitionConsumer) getLastMessageID() (*trackingMessageID, error) {
	res, err := pc.getLastMessageIDAndMarkDeletePosition()
	if err != nil {
		return nil, err
	}
	return res.msgID, err
}

func (pc *partitionConsumer) getLastMessageIDAndMarkDeletePosition() (*getLastMsgIDResult, error) {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to getLastMessageID for the closing or closed consumer")
		return nil, errors.New("failed to getLastMessageID for the closing or closed consumer")
	}
	bo := pc.backoffPolicyFunc()
	request := func() (*getLastMsgIDResult, error) {
		req := &getLastMsgIDRequest{doneCh: make(chan struct{})}
		pc.eventsCh <- req

		// wait for the request to complete
		<-req.doneCh
		res := &getLastMsgIDResult{req.msgID, req.markDeletePosition}
		return res, req.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pc.client.operationTimeout)
	defer cancel()
	res, err := internal.Retry(ctx, request, func(err error) time.Duration {
		nextDelay := bo.Next()
		pc.log.WithError(err).Errorf("Failed to get last message id from broker, retrying in %v...", nextDelay)
		return nextDelay
	})
	if err != nil {
		pc.log.WithError(err).Error("Failed to getLastMessageID")
		return nil, fmt.Errorf("failed to getLastMessageID due to %w", err)
	}

	return res, nil
}

func (pc *partitionConsumer) internalGetLastMessageID(req *getLastMsgIDRequest) {
	defer close(req.doneCh)
	rsp, err := pc.requestGetLastMessageID()
	if err != nil {
		req.err = err
		return
	}
	req.msgID = convertToMessageID(rsp.GetLastMessageId())
	req.markDeletePosition = convertToMessageID(rsp.GetConsumerMarkDeletePosition())
}

func (pc *partitionConsumer) requestGetLastMessageID() (*pb.CommandGetLastMessageIdResponse, error) {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to getLastMessageID closing or closed consumer")
		return nil, errors.New("failed to getLastMessageID closing or closed consumer")
	}

	requestID := pc.client.rpcClient.NewRequestID()
	cmdGetLastMessageID := &pb.CommandGetLastMessageId{
		RequestId:  proto.Uint64(requestID),
		ConsumerId: proto.Uint64(pc.consumerID),
	}
	res, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), requestID,
		pb.BaseCommand_GET_LAST_MESSAGE_ID, cmdGetLastMessageID)
	if err != nil {
		pc.log.WithError(err).Error("Failed to get last message id")
		return nil, err
	}
	return res.Response.GetLastMessageIdResponse, nil
}

func (pc *partitionConsumer) sendIndividualAck(msgID MessageID) *ackRequest {
	ackReq := &ackRequest{
		doneCh:  make(chan struct{}),
		ackType: individualAck,
		msgID:   *msgID.(*trackingMessageID),
	}
	pc.eventsCh <- ackReq
	return ackReq
}

func (pc *partitionConsumer) sendIndividualAckWithTxn(msgID MessageID, txn *transaction) *ackWithTxnRequest {
	ackReq := &ackWithTxnRequest{
		Transaction: txn,
		doneCh:      make(chan struct{}),
		ackType:     individualAck,
		msgID:       *msgID.(*trackingMessageID),
	}
	pc.eventsCh <- ackReq
	return ackReq
}

func (pc *partitionConsumer) AckIDWithResponse(msgID MessageID) error {
	if !checkMessageIDType(msgID) {
		pc.log.Errorf("invalid message id type %T", msgID)
		return fmt.Errorf("invalid message id type %T", msgID)
	}
	return pc.ackID(msgID, true)
}

func (pc *partitionConsumer) AckID(msgID MessageID) error {
	if !checkMessageIDType(msgID) {
		pc.log.Errorf("invalid message id type %T", msgID)
		return fmt.Errorf("invalid message id type %T", msgID)
	}
	return pc.ackID(msgID, false)
}

func (pc *partitionConsumer) AckIDList(msgIDs []MessageID) error {
	if !pc.options.ackWithResponse {
		for _, msgID := range msgIDs {
			if err := pc.AckID(msgID); err != nil {
				return err
			}
		}
		return nil
	}

	chunkedMsgIDs := make([]*chunkMessageID, 0) // we need to remove them after acknowledging
	pendingAcks := make(map[position]*bitset.BitSet)
	validMsgIDs := make([]MessageID, 0, len(msgIDs))

	// They might be complete after the whole for loop
	for _, msgID := range msgIDs {
		if msgID.PartitionIdx() != pc.partitionIdx {
			pc.log.Errorf("%v inconsistent partition index %v (current: %v)", msgID, msgID.PartitionIdx(), pc.partitionIdx)
		} else if msgID.BatchIdx() >= 0 && msgID.BatchSize() > 0 &&
			msgID.BatchIdx() >= msgID.BatchSize() {
			pc.log.Errorf("%v invalid batch index %v (size: %v)", msgID, msgID.BatchIdx(), msgID.BatchSize())
		} else {
			valid := true
			switch convertedMsgID := msgID.(type) {
			case *trackingMessageID:
				position := newPosition(msgID)
				if convertedMsgID.ack() {
					pendingAcks[position] = nil
					pc.metrics.ProcessingTime.Observe(float64(time.Now().UnixNano()-convertedMsgID.receivedTime.UnixNano()) / 1.0e9)
				} else if pc.options.enableBatchIndexAck {
					pendingAcks[position] = convertedMsgID.tracker.getAckBitSet()
				}
			case *chunkMessageID:
				for _, id := range pc.unAckChunksTracker.get(convertedMsgID) {
					pendingAcks[newPosition(id)] = nil
				}
				chunkedMsgIDs = append(chunkedMsgIDs, convertedMsgID)
			case *messageID:
				pendingAcks[newPosition(msgID)] = nil
			default:
				pc.log.Errorf("invalid message id type %T: %v", msgID, msgID)
				valid = false
			}
			if valid {
				validMsgIDs = append(validMsgIDs, msgID)
			}
		}
	}

	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to ack by closing or closed consumer")
		return toAckError(map[error][]MessageID{errors.New("consumer state is closed"): validMsgIDs})
	}

	pc.metrics.AcksCounter.Add(float64(len(validMsgIDs)))

	req := &ackListRequest{
		errCh:  make(chan error),
		msgIDs: toMsgIDDataList(pendingAcks),
	}
	pc.eventsCh <- req
	if err := <-req.errCh; err != nil {
		return toAckError(map[error][]MessageID{err: validMsgIDs})
	}
	for _, id := range chunkedMsgIDs {
		pc.unAckChunksTracker.remove(id)
	}
	for _, id := range msgIDs {
		pc.options.interceptors.OnAcknowledge(pc.parentConsumer, id)
	}
	return nil
}

func toAckError(errorMap map[error][]MessageID) AckError {
	e := AckError{}
	for err, ids := range errorMap {
		for _, id := range ids {
			e[id] = err
		}
	}
	return e
}

func (pc *partitionConsumer) AckIDCumulative(msgID MessageID) error {
	if !checkMessageIDType(msgID) {
		pc.log.Errorf("invalid message id type %T", msgID)
		return fmt.Errorf("invalid message id type %T", msgID)
	}
	return pc.internalAckIDCumulative(msgID, false)
}

func (pc *partitionConsumer) AckIDWithResponseCumulative(msgID MessageID) error {
	if !checkMessageIDType(msgID) {
		pc.log.Errorf("invalid message id type %T", msgID)
		return fmt.Errorf("invalid message id type %T", msgID)
	}
	return pc.internalAckIDCumulative(msgID, true)
}

func (pc *partitionConsumer) isAllowAckCumulative() bool {
	return pc.options.subscriptionType != Shared && pc.options.subscriptionType != KeyShared
}

func (pc *partitionConsumer) internalAckIDCumulative(msgID MessageID, withResponse bool) error {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to ack by closing or closed consumer")
		return errors.New("consumer state is closed")
	}

	if !pc.isAllowAckCumulative() {
		return errors.Wrap(ErrInvalidAck, "cumulative ack is not allowed for the Shared/KeyShared subscription type")
	}

	// chunk message id will be converted to tracking message id
	trackingID := toTrackingMessageID(msgID)
	if trackingID == nil {
		return errors.New("failed to convert trackingMessageID")
	}

	var msgIDToAck *trackingMessageID
	if trackingID.ackCumulative() || pc.options.enableBatchIndexAck {
		msgIDToAck = trackingID
	} else if !trackingID.tracker.hasPrevBatchAcked() {
		// get previous batch message id
		msgIDToAck = trackingID.prev()
		trackingID.tracker.setPrevBatchAcked()
	} else {
		// waiting for all the msgs are acked in this batch
		return nil
	}

	pc.metrics.AcksCounter.Inc()
	pc.metrics.ProcessingTime.Observe(float64(time.Now().UnixNano()-trackingID.receivedTime.UnixNano()) / 1.0e9)

	var ackReq *ackRequest
	if withResponse {
		ackReq = pc.sendCumulativeAck(msgIDToAck)
		<-ackReq.doneCh
	} else {
		pc.ackGroupingTracker.addCumulative(msgIDToAck)
	}

	pc.options.interceptors.OnAcknowledge(pc.parentConsumer, msgID)

	if cmid, ok := msgID.(*chunkMessageID); ok {
		pc.unAckChunksTracker.remove(cmid)
	}

	if ackReq == nil {
		return nil
	}
	return ackReq.err
}

func (pc *partitionConsumer) sendCumulativeAck(msgID MessageID) *ackRequest {
	ackReq := &ackRequest{
		doneCh:  make(chan struct{}),
		ackType: cumulativeAck,
		msgID:   *msgID.(*trackingMessageID),
	}
	pc.eventsCh <- ackReq
	return ackReq
}

func (pc *partitionConsumer) NackID(msgID MessageID) {
	if !checkMessageIDType(msgID) {
		pc.log.Warnf("invalid message id type %T", msgID)
		return
	}

	if cmid, ok := msgID.(*chunkMessageID); ok {
		pc.unAckChunksTracker.nack(cmid)
		return
	}

	trackingID := toTrackingMessageID(msgID)

	pc.nackTracker.Add(trackingID.messageID)
	pc.metrics.NacksCounter.Inc()
}

func (pc *partitionConsumer) NackMsg(msg Message) {
	pc.nackTracker.AddMessage(msg)
	pc.metrics.NacksCounter.Inc()
}

func (pc *partitionConsumer) Redeliver(msgIDs []messageID) {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to redeliver closing or closed consumer")
		return
	}
	pc.eventsCh <- &redeliveryRequest{msgIDs}

	iMsgIDs := make([]MessageID, len(msgIDs))
	for i := range iMsgIDs {
		iMsgIDs[i] = &msgIDs[i]
	}
	pc.options.interceptors.OnNegativeAcksSend(pc.parentConsumer, iMsgIDs)
}

func (pc *partitionConsumer) internalRedeliver(req *redeliveryRequest) {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to redeliver closing or closed consumer")
		return
	}
	msgIDs := req.msgIDs
	pc.log.Debug("Request redelivery after negative ack for messages", msgIDs)

	msgIDDataList := make([]*pb.MessageIdData, len(msgIDs))
	for i := 0; i < len(msgIDs); i++ {
		msgIDDataList[i] = &pb.MessageIdData{
			LedgerId: proto.Uint64(uint64(msgIDs[i].ledgerID)),
			EntryId:  proto.Uint64(uint64(msgIDs[i].entryID)),
		}
	}

	err := pc.client.rpcClient.RequestOnCnxNoWait(pc._getConn(),
		pb.BaseCommand_REDELIVER_UNACKNOWLEDGED_MESSAGES, &pb.CommandRedeliverUnacknowledgedMessages{
			ConsumerId: proto.Uint64(pc.consumerID),
			MessageIds: msgIDDataList,
		})
	if err != nil {
		pc.log.Error("Connection was closed when request redeliver cmd")
	}
}

func (pc *partitionConsumer) getConsumerState() consumerState {
	return consumerState(pc.state.Load())
}

func (pc *partitionConsumer) setConsumerState(state consumerState) {
	pc.state.Store(int32(state))
}

func (pc *partitionConsumer) Close() {

	if pc.getConsumerState() != consumerReady {
		return
	}

	pc.cancelFunc()

	// flush all pending ACK requests and terminate the timer goroutine
	pc.ackGroupingTracker.close()

	// close chunkedMsgCtxMap
	pc.chunkedMsgCtxMap.Close()

	req := &closeRequest{doneCh: make(chan struct{})}
	pc.eventsCh <- req

	// wait for request to finish
	<-req.doneCh
}

func (pc *partitionConsumer) Seek(msgID MessageID) error {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to seek by closing or closed consumer")
		return errors.New("failed to seek by closing or closed consumer")
	}

	if !checkMessageIDType(msgID) {
		pc.log.Errorf("invalid message id type %T", msgID)
		return fmt.Errorf("invalid message id type %T", msgID)
	}

	req := &seekRequest{
		doneCh: make(chan struct{}),
	}
	if cmid, ok := msgID.(*chunkMessageID); ok {
		req.msgID = cmid.firstChunkID
	} else {
		tmid := toTrackingMessageID(msgID)
		req.msgID = tmid.messageID
	}

	pc.ackGroupingTracker.flushAndClean()
	pc.eventsCh <- req

	// wait for the request to complete
	<-req.doneCh
	return req.err
}

func (pc *partitionConsumer) internalSeek(seek *seekRequest) {
	defer close(seek.doneCh)
	seek.err = pc.requestSeek(seek.msgID)
}
func (pc *partitionConsumer) requestSeek(msgID *messageID) error {
	if err := pc.requestSeekWithoutClear(msgID); err != nil {
		return err
	}
	// When the seek operation is successful, it indicates:
	// 1. The broker has reset the cursor and sent a request to close the consumer on the client side.
	//    Since this method is in the same goroutine as the reconnectToBroker,
	//    we can safely clear the messages in the queue (at this point, it won't contain messages after the seek).
	// 2. The startMessageID is reset to ensure accurate judgment when calling hasNext next time.
	//    Since the messages in the queue are cleared here reconnection won't reset startMessageId.
	pc.lastDequeuedMsg = nil
	pc.seekMessageID.set(toTrackingMessageID(msgID))
	pc.clearQueueAndGetNextMessage()
	return nil
}

func (pc *partitionConsumer) requestSeekWithoutClear(msgID *messageID) error {
	state := pc.getConsumerState()
	if state == consumerClosing || state == consumerClosed {
		pc.log.WithField("state", state).Error("failed seek by consumer is closing or has closed")
		return nil
	}

	id := &pb.MessageIdData{}
	err := proto.Unmarshal(msgID.Serialize(), id)
	if err != nil {
		pc.log.WithError(err).Errorf("deserialize message id error: %s", err.Error())
		return err
	}

	requestID := pc.client.rpcClient.NewRequestID()
	cmdSeek := &pb.CommandSeek{
		ConsumerId: proto.Uint64(pc.consumerID),
		RequestId:  proto.Uint64(requestID),
		MessageId:  id,
	}

	_, err = pc.client.rpcClient.RequestOnCnx(pc._getConn(), requestID, pb.BaseCommand_SEEK, cmdSeek)
	if err != nil {
		pc.log.WithError(err).Error("Failed to reset to message id")
		return err
	}
	return nil
}

func (pc *partitionConsumer) SeekByTime(time time.Time) error {
	if state := pc.getConsumerState(); state == consumerClosing || state == consumerClosed {
		pc.log.WithField("state", pc.state).Error("Failed seekByTime by consumer is closing or has closed")
		return errors.New("failed seekByTime by consumer is closing or has closed")
	}
	req := &seekByTimeRequest{
		doneCh:      make(chan struct{}),
		publishTime: time,
	}
	pc.ackGroupingTracker.flushAndClean()
	pc.eventsCh <- req

	// wait for the request to complete
	<-req.doneCh
	return req.err
}

func (pc *partitionConsumer) internalSeekByTime(seek *seekByTimeRequest) {
	defer close(seek.doneCh)

	state := pc.getConsumerState()
	if state == consumerClosing || state == consumerClosed {
		pc.log.WithField("state", pc.state).Error("Failed seekByTime by consumer is closing or has closed")
		return
	}

	requestID := pc.client.rpcClient.NewRequestID()
	cmdSeek := &pb.CommandSeek{
		ConsumerId:         proto.Uint64(pc.consumerID),
		RequestId:          proto.Uint64(requestID),
		MessagePublishTime: proto.Uint64(uint64(seek.publishTime.UnixNano() / int64(time.Millisecond))),
	}

	_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), requestID, pb.BaseCommand_SEEK, cmdSeek)
	if err != nil {
		pc.log.WithError(err).Error("Failed to reset to message publish time")
		seek.err = err
		return
	}
	pc.lastDequeuedMsg = nil
	pc.hasSoughtByTime.Store(true)
	pc.clearQueueAndGetNextMessage()
}

func (pc *partitionConsumer) internalAck(req *ackRequest) {
	defer close(req.doneCh)
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to ack by closing or closed consumer")
		return
	}
	msgID := req.msgID

	messageIDs := make([]*pb.MessageIdData, 1)
	messageIDs[0] = &pb.MessageIdData{
		LedgerId: proto.Uint64(uint64(msgID.ledgerID)),
		EntryId:  proto.Uint64(uint64(msgID.entryID)),
	}
	if pc.options.enableBatchIndexAck && msgID.tracker != nil {
		ackSet := msgID.tracker.toAckSet()
		if ackSet != nil {
			messageIDs[0].AckSet = ackSet
		}
	}

	reqID := pc.client.rpcClient.NewRequestID()
	cmdAck := &pb.CommandAck{
		ConsumerId: proto.Uint64(pc.consumerID),
		MessageId:  messageIDs,
	}

	switch req.ackType {
	case individualAck:
		cmdAck.AckType = pb.CommandAck_Individual.Enum()
	case cumulativeAck:
		cmdAck.AckType = pb.CommandAck_Cumulative.Enum()
	}

	if pc.options.ackWithResponse {
		cmdAck.RequestId = proto.Uint64(reqID)
		_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), reqID, pb.BaseCommand_ACK, cmdAck)
		if err != nil {
			pc.log.WithError(err).Error("Ack with response error")
			req.err = err
		}
		return
	}

	err := pc.client.rpcClient.RequestOnCnxNoWait(pc._getConn(), pb.BaseCommand_ACK, cmdAck)
	if err != nil {
		pc.log.Error("Connection was closed when request ack cmd")
		req.err = err
	}
}

func (pc *partitionConsumer) internalAckList(request *ackListRequest) {
	if request.errCh != nil {
		reqID := pc.client.rpcClient.NewRequestID()
		_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), reqID, pb.BaseCommand_ACK, &pb.CommandAck{
			AckType:    pb.CommandAck_Individual.Enum(),
			ConsumerId: proto.Uint64(pc.consumerID),
			MessageId:  request.msgIDs,
			RequestId:  &reqID,
		})
		request.errCh <- err
		return
	}
	pc.client.rpcClient.RequestOnCnxNoWait(pc._getConn(), pb.BaseCommand_ACK, &pb.CommandAck{
		AckType:    pb.CommandAck_Individual.Enum(),
		ConsumerId: proto.Uint64(pc.consumerID),
		MessageId:  request.msgIDs,
	})
}

func (pc *partitionConsumer) MessageReceived(response *pb.CommandMessage, headersAndPayload internal.Buffer) error {
	pbMsgID := response.GetMessageId()

	reader := internal.NewMessageReader(headersAndPayload)
	brokerMetadata, err := reader.ReadBrokerMetadata()
	if err != nil {
		// todo optimize use more appropriate error codes
		pc.discardCorruptedMessage(pbMsgID, pb.CommandAck_BatchDeSerializeError)
		return err
	}
	msgMeta, err := reader.ReadMessageMetadata()
	if err != nil {
		pc.discardCorruptedMessage(pbMsgID, pb.CommandAck_ChecksumMismatch)
		return err
	}
	decryptedPayload, err := pc.decryptor.Decrypt(headersAndPayload.ReadableSlice(), pbMsgID, msgMeta)
	// error decrypting the payload
	if err != nil {
		// default crypto failure action
		cryptoFailureAction := crypto.ConsumerCryptoFailureActionFail
		if pc.options.decryption != nil {
			cryptoFailureAction = pc.options.decryption.ConsumerCryptoFailureAction
		}

		switch cryptoFailureAction {
		case crypto.ConsumerCryptoFailureActionFail:
			pc.log.Errorf("consuming message failed due to decryption err: %v", err)
			pc.NackID(newTrackingMessageID(int64(pbMsgID.GetLedgerId()), int64(pbMsgID.GetEntryId()), 0, 0, 0, nil))
			return err
		case crypto.ConsumerCryptoFailureActionDiscard:
			pc.discardCorruptedMessage(pbMsgID, pb.CommandAck_DecryptionError)
			return fmt.Errorf("discarding message on decryption error: %w", err)
		case crypto.ConsumerCryptoFailureActionConsume:
			pc.log.Warnf("consuming encrypted message due to error in decryption: %v", err)
			messages := []*message{
				{
					publishTime:  timeFromUnixTimestampMillis(msgMeta.GetPublishTime()),
					eventTime:    timeFromUnixTimestampMillis(msgMeta.GetEventTime()),
					key:          msgMeta.GetPartitionKey(),
					producerName: msgMeta.GetProducerName(),
					properties:   internal.ConvertToStringMap(msgMeta.GetProperties()),
					topic:        pc.topic,
					msgID: newMessageID(
						int64(pbMsgID.GetLedgerId()),
						int64(pbMsgID.GetEntryId()),
						pbMsgID.GetBatchIndex(),
						pc.partitionIdx,
						pbMsgID.GetBatchSize(),
					),
					payLoad:             headersAndPayload.ReadableSlice(),
					schema:              pc.options.schema,
					replicationClusters: msgMeta.GetReplicateTo(),
					replicatedFrom:      msgMeta.GetReplicatedFrom(),
					redeliveryCount:     response.GetRedeliveryCount(),
					encryptionContext:   createEncryptionContext(msgMeta),
					orderingKey:         string(msgMeta.OrderingKey),
				},
			}
			if pc.options.autoReceiverQueueSize {
				pc.incomingMessages.Inc()
				pc.markScaleIfNeed()
			}

			pc.queueCh <- messages
			return nil
		}
	}

	isChunkedMsg := false
	if msgMeta.GetNumChunksFromMsg() > 1 {
		isChunkedMsg = true
	}

	processedPayloadBuffer := internal.NewBufferWrapper(decryptedPayload)
	if isChunkedMsg {
		processedPayloadBuffer = pc.processMessageChunk(processedPayloadBuffer, msgMeta, pbMsgID)
		if processedPayloadBuffer == nil {
			return nil
		}
	}

	var uncompressedHeadersAndPayload internal.Buffer
	// decryption is success, decompress the payload, but only if payload is not empty
	if n := msgMeta.UncompressedSize; n != nil && *n > 0 {
		uncompressedHeadersAndPayload, err = pc.Decompress(msgMeta, processedPayloadBuffer)
		if err != nil {
			pc.discardCorruptedMessage(pbMsgID, pb.CommandAck_DecompressionError)
			return err
		}

		// Reset the reader on the uncompressed buffer
		reader.ResetBuffer(uncompressedHeadersAndPayload)
	}

	numMsgs := 1
	if msgMeta.NumMessagesInBatch != nil {
		numMsgs = int(msgMeta.GetNumMessagesInBatch())
	}

	messages := make([]*message, 0)
	var ackTracker *ackTracker
	// are there multiple messages in this batch?
	if numMsgs > 1 {
		ackTracker = newAckTracker(uint(numMsgs))
	}

	var ackSet *bitset.BitSet
	if response.GetAckSet() != nil {
		ackSetFromResponse := response.GetAckSet()
		buf := make([]uint64, len(ackSetFromResponse))
		for i := 0; i < len(buf); i++ {
			buf[i] = uint64(ackSetFromResponse[i])
		}
		ackSet = bitset.From(buf)
	}

	pc.metrics.MessagesReceived.Add(float64(numMsgs))
	pc.metrics.PrefetchedMessages.Add(float64(numMsgs))

	var (
		bytesReceived   int
		skippedMessages int32
	)
	for i := 0; i < numMsgs; i++ {
		smm, payload, err := reader.ReadMessage()
		if err != nil || payload == nil {
			pc.discardCorruptedMessage(pbMsgID, pb.CommandAck_BatchDeSerializeError)
			return err
		}
		if ackSet != nil && !ackSet.Test(uint(i)) {
			pc.log.Debugf("Ignoring message from %vth message, which has been acknowledged", i)
			skippedMessages++
			continue
		}

		pc.metrics.BytesReceived.Add(float64(len(payload)))
		pc.metrics.PrefetchedBytes.Add(float64(len(payload)))

		trackingMsgID := newTrackingMessageID(
			int64(pbMsgID.GetLedgerId()),
			int64(pbMsgID.GetEntryId()),
			int32(i),
			pc.partitionIdx,
			int32(numMsgs),
			ackTracker)
		// set the consumer so we know how to ack the message id
		trackingMsgID.consumer = pc

		if pc.messageShouldBeDiscarded(trackingMsgID) {
			pc.AckID(trackingMsgID)
			skippedMessages++
			continue
		}

		var msgID MessageID
		if isChunkedMsg {
			ctx := pc.chunkedMsgCtxMap.get(msgMeta.GetUuid())
			if ctx == nil {
				// chunkedMsgCtxMap has closed because of consumer closed
				pc.log.Warnf("get chunkedMsgCtx for chunk with uuid %s failed because consumer has closed",
					msgMeta.Uuid)
				return nil
			}
			cmid := newChunkMessageID(ctx.firstChunkID(), ctx.lastChunkID())
			// set the consumer so we know how to ack the message id
			cmid.consumer = pc
			// clean chunkedMsgCtxMap
			pc.chunkedMsgCtxMap.remove(msgMeta.GetUuid())
			pc.unAckChunksTracker.add(cmid, ctx.chunkedMsgIDs)
			msgID = cmid
		} else {
			msgID = trackingMsgID
		}

		if pc.ackGroupingTracker.isDuplicate(msgID) {
			skippedMessages++
			continue
		}

		var messageIndex *uint64
		var brokerPublishTime *time.Time
		if brokerMetadata != nil {
			if brokerMetadata.Index != nil {
				aux := brokerMetadata.GetIndex() - uint64(numMsgs) + uint64(i) + 1
				messageIndex = &aux
			}
			if brokerMetadata.BrokerTimestamp != nil {
				aux := timeFromUnixTimestampMillis(*brokerMetadata.BrokerTimestamp)
				brokerPublishTime = &aux
			}
		}

		var msg *message
		if smm != nil {
			msg = &message{
				publishTime:         timeFromUnixTimestampMillis(msgMeta.GetPublishTime()),
				eventTime:           timeFromUnixTimestampMillis(smm.GetEventTime()),
				key:                 smm.GetPartitionKey(),
				producerName:        msgMeta.GetProducerName(),
				properties:          internal.ConvertToStringMap(smm.GetProperties()),
				topic:               pc.topic,
				msgID:               msgID,
				payLoad:             payload,
				schema:              pc.options.schema,
				replicationClusters: msgMeta.GetReplicateTo(),
				replicatedFrom:      msgMeta.GetReplicatedFrom(),
				redeliveryCount:     response.GetRedeliveryCount(),
				schemaVersion:       msgMeta.GetSchemaVersion(),
				schemaInfoCache:     pc.schemaInfoCache,
				orderingKey:         string(smm.OrderingKey),
				index:               messageIndex,
				brokerPublishTime:   brokerPublishTime,
			}
		} else {
			msg = &message{
				publishTime:         timeFromUnixTimestampMillis(msgMeta.GetPublishTime()),
				eventTime:           timeFromUnixTimestampMillis(msgMeta.GetEventTime()),
				key:                 msgMeta.GetPartitionKey(),
				producerName:        msgMeta.GetProducerName(),
				properties:          internal.ConvertToStringMap(msgMeta.GetProperties()),
				topic:               pc.topic,
				msgID:               msgID,
				payLoad:             payload,
				schema:              pc.options.schema,
				replicationClusters: msgMeta.GetReplicateTo(),
				replicatedFrom:      msgMeta.GetReplicatedFrom(),
				redeliveryCount:     response.GetRedeliveryCount(),
				schemaVersion:       msgMeta.GetSchemaVersion(),
				schemaInfoCache:     pc.schemaInfoCache,
				orderingKey:         string(msgMeta.GetOrderingKey()),
				index:               messageIndex,
				brokerPublishTime:   brokerPublishTime,
			}
		}

		pc.options.interceptors.BeforeConsume(ConsumerMessage{
			Consumer: pc.parentConsumer,
			Message:  msg,
		})
		messages = append(messages, msg)
		bytesReceived += msg.size()
		if pc.options.autoReceiverQueueSize {
			pc.client.memLimit.ForceReserveMemory(int64(bytesReceived))
			pc.incomingMessages.Add(int32(1))
			pc.markScaleIfNeed()
		}
	}

	if skippedMessages > 0 {
		pc.availablePermits.add(skippedMessages)
	}

	// send messages to the dispatcher
	pc.queueCh <- messages
	return nil
}

func (pc *partitionConsumer) processMessageChunk(compressedPayload internal.Buffer,
	msgMeta *pb.MessageMetadata,
	pbMsgID *pb.MessageIdData) internal.Buffer {
	uuid := msgMeta.GetUuid()
	numChunks := msgMeta.GetNumChunksFromMsg()
	totalChunksSize := int(msgMeta.GetTotalChunkMsgSize())
	chunkID := msgMeta.GetChunkId()
	msgID := &messageID{
		ledgerID:     int64(pbMsgID.GetLedgerId()),
		entryID:      int64(pbMsgID.GetEntryId()),
		batchIdx:     -1,
		partitionIdx: pc.partitionIdx,
	}

	if msgMeta.GetChunkId() == 0 {
		pc.chunkedMsgCtxMap.addIfAbsent(uuid,
			numChunks,
			totalChunksSize,
		)
	}

	ctx := pc.chunkedMsgCtxMap.get(uuid)

	if ctx == nil || ctx.chunkedMsgBuffer == nil || chunkID != ctx.lastChunkedMsgID+1 {
		lastChunkedMsgID := -1
		totalChunks := -1
		if ctx != nil {
			lastChunkedMsgID = int(ctx.lastChunkedMsgID)
			totalChunks = int(ctx.totalChunks)
			ctx.chunkedMsgBuffer.Clear()
		}
		pc.log.Warnf(fmt.Sprintf(
			"Received unexpected chunk messageId %s, last-chunk-id %d, chunkId = %d, total-chunks %d",
			msgID.String(), lastChunkedMsgID, chunkID, totalChunks))
		pc.chunkedMsgCtxMap.remove(uuid)
		pc.availablePermits.inc()
		return nil
	}

	ctx.append(chunkID, msgID, compressedPayload)

	if msgMeta.GetChunkId() != msgMeta.GetNumChunksFromMsg()-1 {
		pc.availablePermits.inc()
		return nil
	}

	return ctx.chunkedMsgBuffer
}

func (pc *partitionConsumer) messageShouldBeDiscarded(msgID *trackingMessageID) bool {
	if pc.startMessageID.get() == nil {
		return false
	}

	// if we start at latest message, we should never discard
	if pc.startMessageID.get().equal(latestMessageID) {
		return false
	}

	if pc.options.startMessageIDInclusive {
		return pc.startMessageID.get().greater(msgID.messageID)
	}

	// Non inclusive
	return pc.startMessageID.get().greaterEqual(msgID.messageID)
}

// create EncryptionContext from message metadata
// this will be used to decrypt the message payload outside of this client
// it is the responsibility of end user to decrypt the payload
// It will be used only when  crypto failure action is set to consume i.e crypto.ConsumerCryptoFailureActionConsume
func createEncryptionContext(msgMeta *pb.MessageMetadata) *EncryptionContext {
	encCtx := EncryptionContext{
		Algorithm:        msgMeta.GetEncryptionAlgo(),
		Param:            msgMeta.GetEncryptionParam(),
		UncompressedSize: int(msgMeta.GetUncompressedSize()),
		BatchSize:        int(msgMeta.GetNumMessagesInBatch()),
	}

	if msgMeta.Compression != nil {
		encCtx.CompressionType = CompressionType(*msgMeta.Compression)
	}

	keyMap := map[string]EncryptionKey{}
	for _, k := range msgMeta.GetEncryptionKeys() {
		metaMap := map[string]string{}
		for _, m := range k.GetMetadata() {
			metaMap[*m.Key] = *m.Value
		}

		keyMap[*k.Key] = EncryptionKey{
			KeyValue: k.GetValue(),
			Metadata: metaMap,
		}
	}

	encCtx.Keys = keyMap
	return &encCtx
}

func (pc *partitionConsumer) ConnectionClosed(closeConsumer *pb.CommandCloseConsumer) {
	// Trigger reconnection in the consumer goroutine
	pc.log.Debug("connection closed and send to connectClosedCh")
	var assignedBrokerURL string
	if closeConsumer != nil {
		assignedBrokerURL = pc.client.selectServiceURL(
			closeConsumer.GetAssignedBrokerServiceUrl(), closeConsumer.GetAssignedBrokerServiceUrlTls())
	}

	select {
	case pc.connectClosedCh <- &connectionClosed{assignedBrokerURL: assignedBrokerURL}:
	default:
		// Reconnect has already been requested so we do not block the
		// connection callback.
	}
}

func (pc *partitionConsumer) SetRedirectedClusterURI(redirectedClusterURI string) {
	pc.redirectedClusterURI = redirectedClusterURI
}

// Flow command gives additional permits to send messages to the consumer.
// A typical consumer implementation will use a queue to accumulate these messages
// before the application is ready to consume them. After the consumer is ready,
// the client needs to give permission to the broker to push messages.
func (pc *partitionConsumer) internalFlow(permits uint32) error {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to redeliver closing or closed consumer")
		return errors.New("consumer closing or closed")
	}
	if permits == 0 {
		return fmt.Errorf("invalid number of permits requested: %d", permits)
	}

	cmdFlow := &pb.CommandFlow{
		ConsumerId:     proto.Uint64(pc.consumerID),
		MessagePermits: proto.Uint32(permits),
	}
	err := pc.client.rpcClient.RequestOnCnxNoWait(pc._getConn(), pb.BaseCommand_FLOW, cmdFlow)
	if err != nil {
		pc.log.Error("Connection was closed when request flow cmd")
		return err
	}

	return nil
}

// dispatcher manages the internal message queue channel
// and manages the flow control
func (pc *partitionConsumer) dispatcher() {
	defer func() {
		pc.log.Debug("exiting dispatch loop")
	}()
	var messages []*message
	for {
		var queueCh chan []*message
		var messageCh chan ConsumerMessage
		var nextMessage ConsumerMessage
		var nextMessageSize int

		// are there more messages to send?
		if len(messages) > 0 {
			nextMessage = ConsumerMessage{
				Consumer: pc.parentConsumer,
				Message:  messages[0],
			}
			nextMessageSize = messages[0].size()

			if !pc.isSeeking.Load() {
				if pc.dlq.shouldSendToDlq(&nextMessage) {
					// pass the message to the DLQ router
					pc.metrics.DlqCounter.Inc()
					messageCh = pc.dlq.Chan()
				} else {
					// pass the message to application channel
					messageCh = pc.messageCh
				}
				pc.metrics.PrefetchedMessages.Dec()
				pc.metrics.PrefetchedBytes.Sub(float64(len(messages[0].payLoad)))
			} else {
				pc.log.Debug("skip dispatching messages when seeking")
			}
		} else {
			queueCh = pc.queueCh
		}

		select {
		case <-pc.closeCh:
			return

		case _, ok := <-pc.connectedCh:
			if !ok {
				return
			}
			pc.log.Debug("dispatcher received connection event")

			messages = nil

			// reset available permits
			pc.availablePermits.reset()

			var initialPermits uint32
			if pc.options.autoReceiverQueueSize {
				initialPermits = uint32(pc.currentQueueSize.Load())
			} else {
				initialPermits = uint32(pc.maxQueueSize)
			}

			pc.log.Debugf("dispatcher requesting initial permits=%d", initialPermits)
			// send initial permits
			if err := pc.internalFlow(initialPermits); err != nil {
				pc.log.WithError(err).Error("unable to send initial permits to broker")
			}

		case _, ok := <-pc.dispatcherSeekingControlCh:
			if !ok {
				return
			}
			pc.log.Debug("received dispatcherSeekingControlCh, set isSeek to true")
			pc.isSeeking.Store(true)

		case msgs, ok := <-queueCh:
			if !ok {
				return
			}

			messages = msgs

		// if the messageCh is nil or the messageCh is full this will not be selected
		case messageCh <- nextMessage:
			// allow this message to be garbage collected
			messages[0] = nil
			messages = messages[1:]

			// for the zeroQueueConsumer, the permits controlled by itself
			if pc.options.receiverQueueSize > 0 {
				pc.availablePermits.inc()
			}

			if pc.options.autoReceiverQueueSize {
				pc.incomingMessages.Dec()
				pc.client.memLimit.ReleaseMemory(int64(nextMessageSize))
				pc.expectMoreIncomingMessages()
			}

		case clearQueueCb := <-pc.clearQueueCh:
			// drain the message queue on any new connection by sending a
			// special nil message to the channel so we know when to stop dropping messages
			var nextMessageInQueue *trackingMessageID
			go func() {
				pc.queueCh <- nil
			}()

			for m := range pc.queueCh {
				// the queue has been drained
				if m == nil {
					break
				} else if nextMessageInQueue == nil {
					nextMessageInQueue = toTrackingMessageID(m[0].msgID)
				}
				if pc.options.autoReceiverQueueSize {
					pc.incomingMessages.Sub(int32(len(m)))
				}
			}

			messages = nil

			clearQueueCb(nextMessageInQueue)
		}
	}
}

const (
	individualAck = iota
	cumulativeAck
)

type ackRequest struct {
	doneCh  chan struct{}
	msgID   trackingMessageID
	ackType int
	err     error
}

type ackListRequest struct {
	errCh  chan error
	msgIDs []*pb.MessageIdData
}

type ackWithTxnRequest struct {
	doneCh      chan struct{}
	msgID       trackingMessageID
	Transaction *transaction
	ackType     int
	err         error
}

type unsubscribeRequest struct {
	doneCh chan struct{}
	force  bool
	err    error
}

type closeRequest struct {
	doneCh chan struct{}
}

type redeliveryRequest struct {
	msgIDs []messageID
}

type getLastMsgIDRequest struct {
	doneCh             chan struct{}
	msgID              *trackingMessageID
	markDeletePosition *trackingMessageID
	err                error
}

type getLastMsgIDResult struct {
	msgID              *trackingMessageID
	markDeletePosition *trackingMessageID
}

type seekRequest struct {
	doneCh chan struct{}
	msgID  *messageID
	err    error
}

type seekByTimeRequest struct {
	doneCh      chan struct{}
	publishTime time.Time
	err         error
}

func (pc *partitionConsumer) runEventsLoop() {
	defer func() {
		pc.log.Debug("exiting events loop")
	}()
	pc.log.Debug("get into runEventsLoop")

	for {
		select {
		case <-pc.closeCh:
			pc.log.Info("close consumer, exit reconnect")
			return
		case connectionClosed := <-pc.connectClosedCh:
			pc.log.Debug("runEventsLoop will reconnect")
			pc.reconnectToBroker(connectionClosed)
		case event := <-pc.eventsCh:
			switch v := event.(type) {
			case *ackRequest:
				pc.internalAck(v)
			case *ackWithTxnRequest:
				pc.internalAckWithTxn(v)
			case *ackListRequest:
				pc.internalAckList(v)
			case *redeliveryRequest:
				pc.internalRedeliver(v)
			case *unsubscribeRequest:
				pc.internalUnsubscribe(v)
			case *getLastMsgIDRequest:
				pc.internalGetLastMessageID(v)
			case *seekRequest:
				pc.internalSeek(v)
			case *seekByTimeRequest:
				pc.internalSeekByTime(v)
			case *closeRequest:
				pc.internalClose(v)
				return
			}
		}
	}
}

func (pc *partitionConsumer) internalClose(req *closeRequest) {
	defer close(req.doneCh)
	state := pc.getConsumerState()
	if state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Consumer is closing or has closed")
		if pc.nackTracker != nil {
			pc.nackTracker.Close()
		}
		return
	}

	if state != consumerReady {
		// this might be redundant but to ensure nack tracker is closed
		if pc.nackTracker != nil {
			pc.nackTracker.Close()
		}
		return
	}

	pc.setConsumerState(consumerClosing)
	pc.log.Infof("Closing consumer=%d", pc.consumerID)

	requestID := pc.client.rpcClient.NewRequestID()
	cmdClose := &pb.CommandCloseConsumer{
		ConsumerId: proto.Uint64(pc.consumerID),
		RequestId:  proto.Uint64(requestID),
	}
	_, err := pc.client.rpcClient.RequestOnCnx(pc._getConn(), requestID, pb.BaseCommand_CLOSE_CONSUMER, cmdClose)
	if err != nil {
		pc.log.WithError(err).Warn("Failed to close consumer")
	} else {
		pc.log.Info("Closed consumer")
	}

	pc.compressionProviders.Range(func(_, v interface{}) bool {
		if provider, ok := v.(compression.Provider); ok {
			provider.Close()
		} else {
			err := fmt.Errorf("unexpected compression provider type: %T", v)
			pc.log.WithError(err).Warn("Failed to close compression provider")
		}
		return true
	})

	pc.setConsumerState(consumerClosed)
	pc._getConn().DeleteConsumeHandler(pc.consumerID)
	if pc.nackTracker != nil {
		pc.nackTracker.Close()
	}
	close(pc.closeCh)
}

func (pc *partitionConsumer) reconnectToBroker(connectionClosed *connectionClosed) {
	if pc.isSeeking.CompareAndSwap(true, false) {
		pc.log.Debug("seek operation triggers reconnection, and reset isSeeking")
	}
	var (
		maxRetry int
	)

	if pc.options.maxReconnectToBroker == nil {
		maxRetry = -1
	} else {
		maxRetry = int(*pc.options.maxReconnectToBroker)
	}
	bo := pc.backoffPolicyFunc()

	var assignedBrokerURL string
	if connectionClosed != nil && connectionClosed.HasURL() {
		assignedBrokerURL = connectionClosed.assignedBrokerURL
	}

	opFn := func() (struct{}, error) {
		if maxRetry == 0 {
			return struct{}{}, nil
		}

		if pc.getConsumerState() != consumerReady {
			// Consumer is already closing
			pc.log.Info("consumer state not ready, exit reconnect")
			return struct{}{}, nil
		}

		err := pc.grabConn(assignedBrokerURL)
		if assignedBrokerURL != "" {
			// Attempt connecting to the assigned broker just once
			assignedBrokerURL = ""
		}
		if err == nil {
			// Successfully reconnected
			pc.log.Info("Reconnected consumer to broker")
			bo.Reset()
			return struct{}{}, nil
		}
		pc.log.WithError(err).Error("Failed to create consumer at reconnect")
		errMsg := err.Error()
		if strings.Contains(errMsg, errMsgTopicNotFound) {
			// when topic is deleted, we should give up reconnection.
			pc.log.Warn("Topic Not Found.")
			return struct{}{}, nil
		}

		if maxRetry > 0 {
			maxRetry--
		}
		pc.metrics.ConsumersReconnectFailure.Inc()
		if maxRetry == 0 || bo.IsMaxBackoffReached() {
			pc.metrics.ConsumersReconnectMaxRetry.Inc()
		}

		return struct{}{}, err
	}
	_, _ = internal.Retry(pc.ctx, opFn, func(_ error) time.Duration {
		delayReconnectTime := bo.Next()
		pc.log.WithFields(log.Fields{
			"assignedBrokerURL":  assignedBrokerURL,
			"delayReconnectTime": delayReconnectTime,
		}).Info("Reconnecting to broker")
		return delayReconnectTime
	})
}

func (pc *partitionConsumer) lookupTopic(brokerServiceURL string) (*internal.LookupResult, error) {
	if len(brokerServiceURL) == 0 {
		lookupService, err := pc.client.rpcClient.LookupService(pc.redirectedClusterURI)
		if err != nil {
			return nil, fmt.Errorf("failed to get lookup service: %w", err)
		}
		lr, err := lookupService.Lookup(pc.topic)
		if err != nil {
			pc.log.WithError(err).Warn("Failed to lookup topic")
			return nil, err
		}

		pc.log.Debug("Lookup result: ", lr)
		return lr, err
	}
	return pc.client.lookupService.GetBrokerAddress(brokerServiceURL, pc._getConn().IsProxied())
}

func (pc *partitionConsumer) grabConn(assignedBrokerURL string) error {
	lr, err := pc.lookupTopic(assignedBrokerURL)
	if err != nil {
		return err
	}

	subType := toProtoSubType(pc.options.subscriptionType)
	initialPosition := toProtoInitialPosition(pc.options.subscriptionInitPos)
	keySharedMeta := toProtoKeySharedMeta(pc.options.keySharedPolicy)
	requestID := pc.client.rpcClient.NewRequestID()

	var pbSchema *pb.Schema

	if pc.options.schema != nil && pc.options.schema.GetSchemaInfo() != nil {
		tmpSchemaType := pb.Schema_Type(int32(pc.options.schema.GetSchemaInfo().Type))
		pbSchema = &pb.Schema{
			Name:       proto.String(pc.options.schema.GetSchemaInfo().Name),
			Type:       &tmpSchemaType,
			SchemaData: []byte(pc.options.schema.GetSchemaInfo().Schema),
			Properties: internal.ConvertFromStringMap(pc.options.schema.GetSchemaInfo().Properties),
		}
		pc.log.Debugf("The partition consumer schema name is: %s", pbSchema.Name)
	} else {
		pc.log.Debug("The partition consumer schema is nil")
	}

	cmdSubscribe := &pb.CommandSubscribe{
		Topic:                      proto.String(pc.topic),
		Subscription:               proto.String(pc.options.subscription),
		SubType:                    subType.Enum(),
		ConsumerId:                 proto.Uint64(pc.consumerID),
		RequestId:                  proto.Uint64(requestID),
		ConsumerName:               proto.String(pc.name),
		PriorityLevel:              nil,
		Durable:                    proto.Bool(pc.options.subscriptionMode == Durable),
		Metadata:                   internal.ConvertFromStringMap(pc.options.metadata),
		SubscriptionProperties:     internal.ConvertFromStringMap(pc.options.subProperties),
		ReadCompacted:              proto.Bool(pc.options.readCompacted),
		Schema:                     pbSchema,
		InitialPosition:            initialPosition.Enum(),
		ReplicateSubscriptionState: proto.Bool(pc.options.replicateSubscriptionState),
		KeySharedMeta:              keySharedMeta,
	}

	if seekMsgID := pc.seekMessageID.get(); seekMsgID != nil {
		pc.startMessageID.set(seekMsgID)
		pc.seekMessageID.set(nil) // Reset seekMessageID to nil to avoid persisting state across reconnects
	} else {
		pc.startMessageID.set(pc.clearReceiverQueue())
	}
	if pc.options.subscriptionMode != Durable {
		// For regular subscriptions the broker will determine the restarting point
		cmdSubscribe.StartMessageId = convertToMessageIDData(pc.startMessageID.get())
	}

	if len(pc.options.metadata) > 0 {
		cmdSubscribe.Metadata = toKeyValues(pc.options.metadata)
	}

	if len(pc.options.subProperties) > 0 {
		cmdSubscribe.SubscriptionProperties = toKeyValues(pc.options.subProperties)
	}

	// force topic creation is enabled by default so
	// we only need to set the flag when disabling it
	if pc.options.disableForceTopicCreation {
		cmdSubscribe.ForceTopicCreation = proto.Bool(false)
	}

	res, err := pc.client.rpcClient.RequestWithCnxKeySuffix(lr.LogicalAddr, lr.PhysicalAddr, pc.cnxKeySuffix, requestID,
		pb.BaseCommand_SUBSCRIBE, cmdSubscribe)

	if err != nil {
		pc.log.WithError(err).Error("Failed to create consumer")
		if err == internal.ErrRequestTimeOut {
			requestID := pc.client.rpcClient.NewRequestID()
			cmdClose := &pb.CommandCloseConsumer{
				ConsumerId: proto.Uint64(pc.consumerID),
				RequestId:  proto.Uint64(requestID),
			}
			_, _ = pc.client.rpcClient.RequestWithCnxKeySuffix(lr.LogicalAddr, lr.PhysicalAddr, pc.cnxKeySuffix, requestID,
				pb.BaseCommand_CLOSE_CONSUMER, cmdClose)
		}
		return err
	}

	if res.Response.ConsumerStatsResponse != nil {
		pc.name = res.Response.ConsumerStatsResponse.GetConsumerName()
	}

	pc._setConn(res.Cnx)
	pc.log.Info("Connected consumer")
	err = pc._getConn().AddConsumeHandler(pc.consumerID, pc)
	if err != nil {
		pc.log.WithError(err).Error("Failed to add consumer handler")
		return err
	}

	msgType := res.Response.GetType()

	switch msgType {
	case pb.BaseCommand_SUCCESS:
		// notify the dispatcher we have connection
		go func() {
			pc.connectedCh <- struct{}{}
		}()
		return nil
	case pb.BaseCommand_ERROR:
		errMsg := res.Response.GetError()
		return fmt.Errorf("%s: %s", errMsg.GetError().String(), errMsg.GetMessage())
	default:
		return newUnexpectedErrMsg(msgType, requestID)
	}
}

func (pc *partitionConsumer) clearQueueAndGetNextMessage() *trackingMessageID {
	if pc.getConsumerState() != consumerReady {
		return nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	var msgID *trackingMessageID

	pc.clearQueueCh <- func(id *trackingMessageID) {
		msgID = id
		wg.Done()
	}

	wg.Wait()
	return msgID
}

/**
 * Clear the internal receiver queue and returns the message id of what was the 1st message in the queue that was
 * not seen by the application
 */
func (pc *partitionConsumer) clearReceiverQueue() *trackingMessageID {
	nextMessageInQueue := pc.clearQueueAndGetNextMessage()

	if pc.startMessageID.get() == nil {
		return pc.startMessageID.get()
	}

	if nextMessageInQueue != nil {
		return getPreviousMessage(nextMessageInQueue)
	} else if pc.lastDequeuedMsg != nil {
		// If the queue was empty we need to restart from the message just after the last one that has been dequeued
		// in the past
		return pc.lastDequeuedMsg
	}
	// No message was received or dequeued by this consumer. Next message would still be the startMessageId
	return pc.startMessageID.get()
}

func getPreviousMessage(mid *trackingMessageID) *trackingMessageID {
	if mid.batchIdx >= 0 {
		return &trackingMessageID{
			messageID: &messageID{
				ledgerID:     mid.ledgerID,
				entryID:      mid.entryID,
				batchIdx:     mid.batchIdx - 1,
				partitionIdx: mid.partitionIdx,
			},
			tracker:      mid.tracker,
			consumer:     mid.consumer,
			receivedTime: mid.receivedTime,
		}
	}

	// Get on previous message in previous entry
	return &trackingMessageID{
		messageID: &messageID{
			ledgerID:     mid.ledgerID,
			entryID:      mid.entryID - 1,
			batchIdx:     mid.batchIdx,
			partitionIdx: mid.partitionIdx,
		},
		tracker:      mid.tracker,
		consumer:     mid.consumer,
		receivedTime: mid.receivedTime,
	}
}

func (pc *partitionConsumer) expectMoreIncomingMessages() {
	if !pc.options.autoReceiverQueueSize {
		return
	}
	if pc.scaleReceiverQueueHint.CompareAndSwap(true, false) {
		oldSize := pc.currentQueueSize.Load()
		maxSize := int32(pc.options.receiverQueueSize)
		newSize := int32(math.Min(float64(maxSize), float64(oldSize*2)))
		usagePercent := pc.client.memLimit.CurrentUsagePercent()
		if usagePercent < receiverQueueExpansionMemThreshold && newSize > oldSize {
			pc.currentQueueSize.CompareAndSwap(oldSize, newSize)
			pc.availablePermits.add(newSize - oldSize)
			pc.log.Debugf("update currentQueueSize from %d -> %d", oldSize, newSize)
		}
	}
}

func (pc *partitionConsumer) markScaleIfNeed() {
	// availablePermits + incomingMessages (messages in queueCh) is the number of prefetched messages
	// The result of auto-scale we expected is currentQueueSize is slightly bigger than prefetched messages
	prev := pc.scaleReceiverQueueHint.Swap(pc.availablePermits.get()+pc.incomingMessages.Load() >=
		pc.currentQueueSize.Load())
	if prev != pc.scaleReceiverQueueHint.Load() {
		pc.log.Debugf("update scaleReceiverQueueHint from %t -> %t", prev, pc.scaleReceiverQueueHint.Load())
	}
}

func (pc *partitionConsumer) shrinkReceiverQueueSize() {
	if !pc.options.autoReceiverQueueSize {
		return
	}

	oldSize := pc.currentQueueSize.Load()
	minSize := int32(math.Min(float64(initialReceiverQueueSize), float64(pc.options.receiverQueueSize)))
	newSize := int32(math.Max(float64(minSize), float64(oldSize/2)))
	if newSize < oldSize {
		pc.currentQueueSize.CompareAndSwap(oldSize, newSize)
		pc.availablePermits.add(newSize - oldSize)
		pc.log.Debugf("update currentQueueSize from %d -> %d", oldSize, newSize)
	}
}

func (pc *partitionConsumer) Decompress(msgMeta *pb.MessageMetadata, payload internal.Buffer) (internal.Buffer, error) {
	providerEntry, ok := pc.compressionProviders.Load(msgMeta.GetCompression())
	if !ok {
		newProvider, err := pc.initializeCompressionProvider(msgMeta.GetCompression())
		if err != nil {
			pc.log.WithError(err).Error("Failed to decompress message.")
			return nil, err
		}

		var loaded bool
		providerEntry, loaded = pc.compressionProviders.LoadOrStore(msgMeta.GetCompression(), newProvider)
		if loaded {
			// another thread already loaded this provider, so close the one we just initialized
			newProvider.Close()
		}
	}
	provider, ok := providerEntry.(compression.Provider)
	if !ok {
		err := fmt.Errorf("unexpected compression provider type: %T", providerEntry)
		pc.log.WithError(err).Error("Failed to decompress message.")
		return nil, err
	}

	uncompressed, err := provider.Decompress(nil, payload.ReadableSlice(), int(msgMeta.GetUncompressedSize()))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress message: %w, compression type: %d, payload size: %d, "+
			"uncompressed size: %d",
			err,
			msgMeta.GetCompression(),
			len(payload.ReadableSlice()),
			msgMeta.GetUncompressedSize())
	}

	return internal.NewBufferWrapper(uncompressed), nil
}

func (pc *partitionConsumer) initializeCompressionProvider(
	compressionType pb.CompressionType) (compression.Provider, error) {
	switch compressionType {
	case pb.CompressionType_NONE:
		return compression.NewNoopProvider(), nil
	case pb.CompressionType_ZLIB:
		return compression.NewZLibProvider(), nil
	case pb.CompressionType_LZ4:
		return compression.NewLz4Provider(), nil
	case pb.CompressionType_ZSTD:
		return compression.NewZStdProvider(compression.Default), nil
	}

	return nil, fmt.Errorf("unsupported compression type: %v", compressionType)
}

func (pc *partitionConsumer) discardCorruptedMessage(msgID *pb.MessageIdData,
	validationError pb.CommandAck_ValidationError) {
	if state := pc.getConsumerState(); state == consumerClosed || state == consumerClosing {
		pc.log.WithField("state", state).Error("Failed to discardCorruptedMessage " +
			"by closing or closed consumer")
		return
	}
	pc.log.WithFields(log.Fields{
		"msgID":           msgID,
		"validationError": validationError,
	}).Error("Discarding corrupted message")

	err := pc.client.rpcClient.RequestOnCnxNoWait(pc._getConn(),
		pb.BaseCommand_ACK, &pb.CommandAck{
			ConsumerId:      proto.Uint64(pc.consumerID),
			MessageId:       []*pb.MessageIdData{msgID},
			AckType:         pb.CommandAck_Individual.Enum(),
			ValidationError: validationError.Enum(),
		})
	if err != nil {
		pc.log.Error("Connection was closed when request ack cmd")
	}
	pc.availablePermits.inc()
}

func (pc *partitionConsumer) hasNext() bool {

	// If a seek by time has been performed, then the `startMessageId` becomes irrelevant.
	// We need to compare `markDeletePosition` and `lastMessageId`,
	// and then reset `startMessageID` to `markDeletePosition`.
	if pc.lastDequeuedMsg == nil && pc.hasSoughtByTime.CompareAndSwap(true, false) {
		res, err := pc.getLastMessageIDAndMarkDeletePosition()
		if err != nil {
			pc.log.WithError(err).Error("Failed to get last message id")
			pc.hasSoughtByTime.CompareAndSwap(false, true)
			return false
		}
		pc.lastMessageInBroker = res.msgID
		pc.startMessageID.set(res.markDeletePosition)
		// We only care about comparing ledger ids and entry ids as mark delete position
		// doesn't have other ids such as batch index
		compareResult := pc.lastMessageInBroker.messageID.compareLedgerAndEntryID(pc.startMessageID.get().messageID)
		return compareResult > 0 || (pc.options.startMessageIDInclusive && compareResult == 0)
	}

	if pc.lastMessageInBroker != nil && pc.hasMoreMessages() {
		return true
	}

	lastMsgID, err := pc.getLastMessageID()
	if err != nil {
		return false
	}
	pc.lastMessageInBroker = lastMsgID

	return pc.hasMoreMessages()
}

func (pc *partitionConsumer) hasMoreMessages() bool {
	if pc.lastDequeuedMsg != nil {
		return pc.lastMessageInBroker.isEntryIDValid() && pc.lastMessageInBroker.greater(pc.lastDequeuedMsg.messageID)
	}

	if pc.options.startMessageIDInclusive {
		return pc.lastMessageInBroker.isEntryIDValid() &&
			pc.lastMessageInBroker.greaterEqual(pc.startMessageID.get().messageID)
	}

	// Non-inclusive
	return pc.lastMessageInBroker.isEntryIDValid() &&
		pc.lastMessageInBroker.greater(pc.startMessageID.get().messageID)
}

// _setConn sets the internal connection field of this partition consumer atomically.
// Note: should only be called by this partition consumer when a new connection is available.
func (pc *partitionConsumer) _setConn(conn internal.Connection) {
	pc.conn.Store(&conn)
}

// _getConn returns internal connection field of this partition consumer atomically.
// Note: should only be called by this partition consumer before attempting to use the connection
func (pc *partitionConsumer) _getConn() internal.Connection {
	// Invariant: The conn must be non-nill for the lifetime of the partitionConsumer.
	//            For this reason we leave this cast unchecked and panic() if the
	//            invariant is broken
	return *pc.conn.Load()
}

func convertToMessageIDData(msgID *trackingMessageID) *pb.MessageIdData {
	if msgID == nil {
		return nil
	}

	return &pb.MessageIdData{
		LedgerId: proto.Uint64(uint64(msgID.ledgerID)),
		EntryId:  proto.Uint64(uint64(msgID.entryID)),
	}
}

func convertToMessageID(id *pb.MessageIdData) *trackingMessageID {
	if id == nil {
		return nil
	}

	msgID := &trackingMessageID{
		messageID: &messageID{
			ledgerID:  int64(id.GetLedgerId()),
			entryID:   int64(id.GetEntryId()),
			batchIdx:  id.GetBatchIndex(),
			batchSize: id.GetBatchSize(),
		},
	}
	if msgID.batchIdx == -1 {
		msgID.batchIdx = 0
	}

	return msgID
}

type chunkedMsgCtx struct {
	totalChunks      int32
	chunkedMsgBuffer internal.Buffer
	lastChunkedMsgID int32
	chunkedMsgIDs    []*messageID
	receivedTime     int64

	mu sync.Mutex
}

func newChunkedMsgCtx(numChunksFromMsg int32, totalChunkMsgSize int) *chunkedMsgCtx {
	return &chunkedMsgCtx{
		totalChunks:      numChunksFromMsg,
		chunkedMsgBuffer: internal.NewBuffer(totalChunkMsgSize),
		lastChunkedMsgID: -1,
		chunkedMsgIDs:    make([]*messageID, numChunksFromMsg),
		receivedTime:     time.Now().Unix(),
	}
}

func (c *chunkedMsgCtx) append(chunkID int32, msgID *messageID, partPayload internal.Buffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunkedMsgIDs[chunkID] = msgID
	c.chunkedMsgBuffer.Write(partPayload.ReadableSlice())
	c.lastChunkedMsgID = chunkID
}

func (c *chunkedMsgCtx) firstChunkID() *messageID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.chunkedMsgIDs) == 0 {
		return nil
	}
	return c.chunkedMsgIDs[0]
}

func (c *chunkedMsgCtx) lastChunkID() *messageID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.chunkedMsgIDs) == 0 {
		return nil
	}
	return c.chunkedMsgIDs[len(c.chunkedMsgIDs)-1]
}

func (c *chunkedMsgCtx) discard(pc *partitionConsumer) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, mid := range c.chunkedMsgIDs {
		if mid == nil {
			continue
		}
		pc.log.Info("Removing chunk message-id", mid.String())
		tmid := toTrackingMessageID(mid)
		pc.AckID(tmid)
	}
}

type chunkedMsgCtxMap struct {
	chunkedMsgCtxs map[string]*chunkedMsgCtx
	pendingQueue   *list.List
	maxPending     int
	pc             *partitionConsumer
	mu             sync.Mutex
	closed         bool
}

func newChunkedMsgCtxMap(maxPending int, pc *partitionConsumer) *chunkedMsgCtxMap {
	return &chunkedMsgCtxMap{
		chunkedMsgCtxs: make(map[string]*chunkedMsgCtx, maxPending),
		pendingQueue:   list.New(),
		maxPending:     maxPending,
		pc:             pc,
		mu:             sync.Mutex{},
	}
}

func (c *chunkedMsgCtxMap) addIfAbsent(uuid string, totalChunks int32, totalChunkMsgSize int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if _, ok := c.chunkedMsgCtxs[uuid]; !ok {
		c.chunkedMsgCtxs[uuid] = newChunkedMsgCtx(totalChunks, totalChunkMsgSize)
		c.pendingQueue.PushBack(uuid)
		go c.discardChunkIfExpire(uuid, true, c.pc.options.expireTimeOfIncompleteChunk)
	}
	if c.maxPending > 0 && c.pendingQueue.Len() > c.maxPending {
		go c.discardOldestChunkMessage(c.pc.options.autoAckIncompleteChunk)
	}
}

func (c *chunkedMsgCtxMap) get(uuid string) *chunkedMsgCtx {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return c.chunkedMsgCtxs[uuid]
}

func (c *chunkedMsgCtxMap) remove(uuid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	delete(c.chunkedMsgCtxs, uuid)
	e := c.pendingQueue.Front()
	for ; e != nil; e = e.Next() {
		if e.Value.(string) == uuid {
			c.pendingQueue.Remove(e)
			break
		}
	}
}

func (c *chunkedMsgCtxMap) discardOldestChunkMessage(autoAck bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || (c.maxPending > 0 && c.pendingQueue.Len() <= c.maxPending) {
		return
	}
	oldest := c.pendingQueue.Front().Value.(string)
	ctx, ok := c.chunkedMsgCtxs[oldest]
	if !ok {
		return
	}
	if autoAck {
		ctx.discard(c.pc)
	}
	delete(c.chunkedMsgCtxs, oldest)
	c.pc.log.Infof("Chunked message [%s] has been removed from chunkedMsgCtxMap", oldest)
}

func (c *chunkedMsgCtxMap) discardChunkMessage(uuid string, autoAck bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	ctx, ok := c.chunkedMsgCtxs[uuid]
	if !ok {
		return
	}
	if autoAck {
		ctx.discard(c.pc)
	}
	delete(c.chunkedMsgCtxs, uuid)
	e := c.pendingQueue.Front()
	for ; e != nil; e = e.Next() {
		if e.Value.(string) == uuid {
			c.pendingQueue.Remove(e)
			break
		}
	}
	c.pc.log.Infof("Chunked message [%s] has been removed from chunkedMsgCtxMap", uuid)
}

func (c *chunkedMsgCtxMap) discardChunkIfExpire(uuid string, autoAck bool, expire time.Duration) {
	timer := time.NewTimer(expire)
	<-timer.C
	c.discardChunkMessage(uuid, autoAck)
}

func (c *chunkedMsgCtxMap) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

type unAckChunksTracker struct {
	// TODO: use hash code of chunkMessageID as the key
	chunkIDs map[chunkMessageID][]*messageID
	pc       *partitionConsumer
	mu       sync.Mutex
}

func newUnAckChunksTracker(pc *partitionConsumer) *unAckChunksTracker {
	return &unAckChunksTracker{
		chunkIDs: make(map[chunkMessageID][]*messageID),
		pc:       pc,
	}
}

func (u *unAckChunksTracker) add(cmid *chunkMessageID, ids []*messageID) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.chunkIDs[*cmid] = ids
}

func (u *unAckChunksTracker) get(cmid *chunkMessageID) []*messageID {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.chunkIDs[*cmid]
}

func (u *unAckChunksTracker) remove(cmid *chunkMessageID) {
	u.mu.Lock()
	defer u.mu.Unlock()

	delete(u.chunkIDs, *cmid)
}

func (u *unAckChunksTracker) ack(cmid *chunkMessageID) error {
	return u.ackWithTxn(cmid, nil)
}

func (u *unAckChunksTracker) ackWithTxn(cmid *chunkMessageID, txn Transaction) error {
	ids := u.get(cmid)
	for _, id := range ids {
		var err error
		if txn == nil {
			err = u.pc.AckID(id)
		} else {
			err = u.pc.AckIDWithTxn(id, txn)
		}
		if err != nil {
			return err
		}
	}
	u.remove(cmid)
	return nil
}

func (u *unAckChunksTracker) nack(cmid *chunkMessageID) {
	ids := u.get(cmid)
	for _, id := range ids {
		u.pc.NackID(id)
	}
	u.remove(cmid)
}
