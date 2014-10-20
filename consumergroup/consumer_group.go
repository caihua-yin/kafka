package consumergroup

import (
	"errors"
	// "math"
	// "sort"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	// "github.com/samuel/go-zookeeper/zk"
)

var (
	DiscardCommit = errors.New("sarama: commit discarded")
	NoCheckout    = errors.New("sarama: not checkout")
)

const (
	REBALANCE_START uint8 = iota + 1
	REBALANCE_OK
	REBALANCE_ERROR
)

type ConsumerGroupConfig struct {
	// The Zookeeper read timeout
	ZookeeperTimeout time.Duration

	// Zookeeper chroot to use. Should not include a trailing slash.
	// Leave this empty for to not set a chroot.
	ZookeeperChroot string

	KafkaClientConfig   *sarama.ClientConfig   // This will be passed to Sarama when creating a new sarama.Client
	KafkaConsumerConfig *sarama.ConsumerConfig // This will be passed to Sarama when creating a new sarama.Consumer

	ChannelBufferSize int
}

func NewConsumerGroupConfig() *ConsumerGroupConfig {
	return &ConsumerGroupConfig{
		ZookeeperTimeout:    1 * time.Second,
		KafkaClientConfig:   sarama.NewClientConfig(),
		KafkaConsumerConfig: sarama.NewConsumerConfig(),
	}
}

func (cgc *ConsumerGroupConfig) Validate() error {
	if cgc.ZookeeperTimeout <= 0 {
		return errors.New("ZookeeperTimeout should have a duration > 0")
	}

	if cgc.KafkaClientConfig == nil {
		return errors.New("KafkaClientConfig is not set!")
	} else if err := cgc.KafkaClientConfig.Validate(); err != nil {
		return err
	}

	if cgc.KafkaConsumerConfig == nil {
		return errors.New("KafkaConsumerConfig is not set!")
	} else if err := cgc.KafkaConsumerConfig.Validate(); err != nil {
		return err
	}

	return nil
}

type ConsumerGroup struct {
	id, name string

	config *ConsumerGroupConfig

	client *sarama.Client
	zk     *ZK

	wg sync.WaitGroup

	events  chan *sarama.ConsumerEvent
	stopper chan struct{}
}

// Connects to a consumer group, using Zookeeper for auto-discovery
func JoinConsumerGroup(name string, topics []string, zookeeper []string, config *ConsumerGroupConfig) (cg *ConsumerGroup, err error) {

	if name == "" {
		return nil, sarama.ConfigurationError("Empty consumergroup name")
	}

	if len(topics) == 0 {
		return nil, sarama.ConfigurationError("No topics provided")
	}

	if len(zookeeper) == 0 {
		return nil, errors.New("You need to provide at least one zookeeper node address!")
	}

	if config == nil {
		config = NewConsumerGroupConfig()
	}

	// Validate configuration
	if err = config.Validate(); err != nil {
		return
	}

	var zk *ZK
	if zk, err = NewZK(zookeeper, config.ZookeeperChroot, config.ZookeeperTimeout); err != nil {
		return
	}

	var kafkaBrokers []string
	if kafkaBrokers, err = zk.Brokers(); err != nil {
		return
	}

	var client *sarama.Client
	if client, err = sarama.NewClient(name, kafkaBrokers, config.KafkaClientConfig); err != nil {
		return
	}

	var consumerID string
	consumerID, err = generateConsumerID()
	if err != nil {
		return
	}

	// Register consumer group
	if err = zk.RegisterGroup(name); err != nil {
		return
	}

	// Register itself with zookeeper
	sarama.Logger.Printf("Registering consumer %s for group %s\n", consumerID, name)
	if err = zk.RegisterConsumer(name, consumerID, topics); err != nil {
		return
	}

	group := &ConsumerGroup{
		id:      consumerID,
		name:    name,
		config:  config,
		client:  client,
		zk:      zk,
		events:  make(chan *sarama.ConsumerEvent, config.ChannelBufferSize),
		stopper: make(chan struct{}),
	}

	for _, topic := range topics {
		group.wg.Add(1)
		go group.topicConsumer(topic, group.events)
	}

	return group, nil
}

func (cg *ConsumerGroup) Events() <-chan *sarama.ConsumerEvent {
	return cg.events
}

func (cg *ConsumerGroup) Close() error {
	defer cg.zk.Close()
	close(cg.stopper)
	cg.wg.Wait()

	if err := cg.zk.DeregisterConsumer(cg.name, cg.id); err != nil {
		sarama.Logger.Printf("FAILED deregistering consumer %s for group %s\n", cg.id, cg.name)
		return err
	} else {
		sarama.Logger.Printf("Deregistering consumer %s for group %s\n", cg.id, cg.name)
	}

	close(cg.events)
	return nil
}

func (cg *ConsumerGroup) topicConsumer(topic string, events chan<- *sarama.ConsumerEvent) (err error) {
	defer cg.wg.Done()

	sarama.Logger.Printf("[%s/%s] Started topic consumer for %s\n", cg.name, cg.id, topic)

	// Fetch a list of partition IDs
	var pids []int32
	if pids, err = cg.client.Partitions(topic); err != nil {
		return
	}

	var wg sync.WaitGroup
	for _, pid := range pids {
		wg.Add(1)
		go cg.partitionConsumer(topic, pid, events, &wg)
	}

	wg.Wait()
	sarama.Logger.Printf("[%s/%s] Stopped topic consumer for %s\n", cg.name, cg.id, topic)
	return nil
}

func (cg *ConsumerGroup) partitionConsumer(topic string, partition int32, events chan<- *sarama.ConsumerEvent, wg *sync.WaitGroup) error {
	defer wg.Done()

	consumer, err := sarama.NewConsumer(cg.client, topic, partition, cg.name, cg.config.KafkaConsumerConfig)
	if err != nil {
		return err
	}
	defer consumer.Close()

	sarama.Logger.Printf("[%s/%s] Started partition consumer for %s/%d\n", cg.name, cg.id, topic, partition)

	partitionEvents := consumer.Events()
partitionConsumerLoop:
	for {
		select {
		case event := <-partitionEvents:
			// sarama.Logger.Printf("[%s/%s] Event on partition consumer for %s/%d\n", cg.name, cg.id, topic, partition)
			events <- event
		case <-cg.stopper:
			sarama.Logger.Printf("[%s/%s] Stopping partition consumer for %s/%d\n", cg.name, cg.id, topic, partition)
			break partitionConsumerLoop
		}
	}
	return nil
}

// 	// Register itself with zookeeper
// 	if err = zoo.RegisterConsumer(group.name, group.id, group.topic); err != nil {
// 		return nil, err
// 	}

// 	group.wg.Add(2)
// 	go group.signalLoop()
// 	go group.eventLoop()
// 	return group, nil
// }

// // Checkout applies a callback function to a single partition consumer.
// // The latest consumer offset is automatically comitted to zookeeper if successful.
// // The callback may return a DiscardCommit error to skip the commit silently.
// // Returns an error if any, but may also return a NoCheckout error to indicate
// // that no partition was available. You should add an artificial delay keep your CPU cool.
// func (cg *ConsumerGroup) Checkout(callback func(*PartitionConsumer) error) error {
// 	select {
// 	case <-cg.stopper:
// 		return NoCheckout
// 	case cg.checkout <- true:
// 	}

// 	var claimed *PartitionConsumer
// 	select {
// 	case claimed = <-cg.claimed:
// 	case <-cg.stopper:
// 		return NoCheckout
// 	}

// 	if claimed == nil {
// 		return NoCheckout
// 	}

// 	err := callback(claimed)
// 	if err == DiscardCommit {
// 		err = nil
// 	} else if err == nil && claimed.offset > 0 {
// 		sarama.Logger.Printf("Committing partition %d offset %d", claimed.partition, claimed.offset)
// 		err = cg.Commit(claimed.partition, claimed.offset)
// 	}
// 	return err
// }

// func (cg *ConsumerGroup) Stream() <-chan *sarama.ConsumerEvent {
// 	return cg.events
// }

// func (cg *ConsumerGroup) eventLoop() {
// 	defer cg.wg.Done()
// EventLoop:
// 	for {
// 		select {
// 		case <-cg.stopper:
// 			close(cg.events)
// 			break EventLoop

// 		default:
// 			cg.Checkout(func(pc *PartitionConsumer) error {
// 				sarama.Logger.Printf("Start consuming partition %d...", pc.partition)
// 				return pc.Fetch(cg.events, pc.group.config.CheckoutInterval, cg.stopper)
// 			})
// 		}

// 	}
// }

// // Commit manually commits an offset for a partition
// func (cg *ConsumerGroup) Commit(partition int32, offset int64) error {
// 	return cg.zoo.Commit(cg.name, cg.topic, partition, offset)
// }

// // Offset manually retrives an offset for a partition
// func (cg *ConsumerGroup) Offset(partition int32) (int64, error) {
// 	return cg.zoo.Offset(cg.name, cg.topic, partition)
// }

// // Claims returns the claimed partitions
// func (cg *ConsumerGroup) Claims() []int32 {
// 	res := make([]int32, 0, len(cg.claims))
// 	for _, claim := range cg.claims {
// 		res = append(res, claim.partition)
// 	}
// 	return res
// }

// // Close closes the consumer group
// func (cg *ConsumerGroup) Close() error {
// 	close(cg.stopper)
// 	cg.wg.Wait()
// 	cg.zoo.Close()
// 	return nil
// }

// // Background signal loop
// func (cg *ConsumerGroup) signalLoop() {
// 	defer cg.wg.Done()
// 	defer cg.releaseClaims()
// 	for {
// 		// If we have no zk handle, rebalance
// 		if cg.zkchange == nil {
// 			if err := cg.rebalance(); err != nil && cg.listener != nil {
// 				select {
// 				case cg.listener <- &Notification{Type: REBALANCE_ERROR, Src: cg, Err: err}:
// 				case <-cg.stopper:
// 					return
// 				}
// 			}
// 		}

// 		// If rebalance failed, check if we had a stop signal, then try again
// 		if cg.zkchange == nil {
// 			select {
// 			case <-cg.stopper:
// 				return
// 			case <-time.After(time.Millisecond):
// 				// Continue
// 			}
// 			continue
// 		}

// 		// If rebalance worked, wait for a stop signal or a zookeeper change or a fetch-request
// 		select {
// 		case <-cg.stopper:
// 			return
// 		case <-cg.force:
// 			cg.zkchange = nil
// 		case <-cg.zkchange:
// 			cg.zkchange = nil
// 		case <-cg.checkout:
// 			select {
// 			case cg.claimed <- cg.nextConsumer():
// 			case <-cg.stopper:
// 				return
// 			}
// 		}
// 	}
// }

// func (cg *ConsumerGroup) EventsBehindLatest() (map[int32]int64, error) {
// 	result := make(map[int32]int64, 0)
// 	latest, offsetErr := cg.latestOffsets()
// 	if offsetErr != nil {
// 		return nil, offsetErr
// 	}

// 	for _, pc := range cg.claims {
// 		latestOffset := latest[pc.partition]
// 		if latestOffset != 0 && pc.offset != 0 {
// 			result[pc.partition] = latestOffset - pc.offset
// 		}
// 	}

// 	return result, nil
// }

// /**********************************************************************
//  * PRIVATE
//  **********************************************************************/

// // Checkout a claimed partition consumer
// func (cg *ConsumerGroup) nextConsumer() *PartitionConsumer {
// 	if len(cg.claims) < 1 {
// 		return nil
// 	}

// 	shift := cg.claims[0]
// 	cg.claims = append(cg.claims[1:], shift)
// 	return shift
// }

// // Start a rebalance cycle
// func (cg *ConsumerGroup) rebalance() (err error) {
// 	var cids []string
// 	var pids []int32

// 	if cg.listener != nil {
// 		cg.listener <- &Notification{Type: REBALANCE_START, Src: cg}
// 	}

// 	// Fetch a list of consumers and listen for changes
// 	if cids, cg.zkchange, err = cg.zoo.Consumers(cg.name); err != nil {
// 		cg.zkchange = nil
// 		return
// 	}

// 	// Fetch a list of partition IDs
// 	if pids, err = cg.client.Partitions(cg.topic); err != nil {
// 		cg.zkchange = nil
// 		return
// 	}

// 	// Get leaders for each partition ID
// 	parts := make(partitionSlice, len(pids))
// 	for i, pid := range pids {
// 		var broker *sarama.Broker
// 		if broker, err = cg.client.Leader(cg.topic, pid); err != nil {
// 			cg.zkchange = nil
// 			return
// 		}
// 		defer broker.Close()
// 		parts[i] = partitionLeader{id: pid, leader: broker.Addr()}
// 	}

// 	if err = cg.makeClaims(cids, parts); err != nil {
// 		cg.zkchange = nil
// 		cg.releaseClaims()
// 		return
// 	}
// 	return
// }

// func (cg *ConsumerGroup) makeClaims(cids []string, parts partitionSlice) error {
// 	cg.releaseClaims()

// 	for _, part := range cg.claimRange(cids, parts) {
// 		pc, err := NewPartitionConsumer(cg, part.id)
// 		if err != nil {
// 			return err
// 		}

// 		err = cg.zoo.Claim(cg.name, cg.topic, pc.partition, cg.id)
// 		if err != nil {
// 			return err
// 		}

// 		cg.claims = append(cg.claims, pc)
// 	}

// 	if cg.listener != nil {
// 		cg.listener <- &Notification{Type: REBALANCE_OK, Src: cg}
// 	}
// 	return nil
// }

// // Determine the partititon numbers to claim
// func (cg *ConsumerGroup) claimRange(cids []string, parts partitionSlice) partitionSlice {
// 	sort.Strings(cids)
// 	sort.Sort(parts)

// 	cpos := sort.SearchStrings(cids, cg.id)
// 	clen := len(cids)
// 	plen := len(parts)
// 	if cpos >= clen || cpos >= plen {
// 		return make(partitionSlice, 0)
// 	}

// 	step := int(math.Ceil(float64(plen) / float64(clen)))
// 	if step < 1 {
// 		step = 1
// 	}

// 	last := (cpos + 1) * step
// 	first := cpos * step
// 	if last > plen {
// 		last = plen
// 	}

// 	if first > plen {
// 		first = plen
// 	}

// 	return parts[first:last]
// }

// // Releases all claims
// func (cg *ConsumerGroup) releaseClaims() {
// 	for _, pc := range cg.claims {
// 		sarama.Logger.Printf("Releasing claim for partition %d...\n", pc.partition)
// 		pc.Close()
// 		cg.zoo.Release(cg.name, cg.topic, pc.partition, cg.id)
// 	}
// 	cg.claims = cg.claims[:0]
// }

// func (cg *ConsumerGroup) latestOffsets() (map[int32]int64, error) {
// 	offsets := make(map[int32]int64)
// 	for _, pc := range cg.claims {
// 		currentOffset, err := cg.client.GetOffset(cg.topic, pc.partition, sarama.LatestOffsets)
// 		if err != nil {
// 			return nil, err
// 		}

// 		offsets[pc.partition] = currentOffset
// 	}
// 	return offsets, nil
// }
