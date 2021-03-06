package kafka

import (
	"fmt"

	"github.com/Shopify/sarama"
	cluster "github.com/bsm/sarama-cluster"
)

type groupConsumer struct {
	brokers  []string
	config   *cluster.Config
	consumer clusterConsumer

	group        string
	partitionMap map[int32]bool
	addPartition chan int32

	events chan Event
	stop   chan bool
	done   chan bool
}

func newGroupConsumer(brokers []string, group string, events chan Event, config *cluster.Config) (*groupConsumer, error) {
	return &groupConsumer{
		group:        group,
		brokers:      brokers,
		config:       config,
		partitionMap: make(map[int32]bool),
		addPartition: make(chan int32, 2048),
		events:       events,
		stop:         make(chan bool),
		done:         make(chan bool),
	}, nil
}

func (c *groupConsumer) Close() error {
	close(c.stop)
	<-c.done
	if err := c.consumer.Close(); err != nil {
		return fmt.Errorf("Failed to close consumer: %v", err)
	}
	return nil
}

func (c *groupConsumer) Subscribe(topics map[string]int64) error {
	var ts []string
	for t := range topics {
		ts = append(ts, string(t))
	}
	upConsumer, err := cluster.NewConsumer(c.brokers, c.group, ts, c.config)
	if err != nil {
		return err
	}
	c.consumer = upConsumer

	go c.run()

	return nil
}

func (c *groupConsumer) waitForNotification() bool {
	for {
		select {
		case n := <-c.consumer.Notifications():
			return c.handleNotification(n)

		case err := <-c.consumer.Errors():
			select {
			case c.events <- &Error{err}:
			case <-c.stop:
				return false
			}

		case <-c.stop:
			return false
		}
	}
}

func (c *groupConsumer) handleNotification(n *cluster.Notification) bool {
	// save partition map
	m := c.partitionMap
	c.partitionMap = make(map[int32]bool)

	// create assignment and update partitionMap
	a := make(Assignment)
	for _, v := range n.Current {
		for _, p := range v {
			a[p] = sarama.OffsetNewest

			// remember whether partition was added using m[p]
			c.partitionMap[p] = m[p]
		}

		break // copartitioned topics
	}

	// send assignment
	select {
	case c.events <- &a:
		return true
	case <-c.stop:
		return false
	}
}

// returns true if all partitions are registered. otherwise false
func (c *groupConsumer) partitionsRegistered() bool {
	for _, v := range c.partitionMap {
		if !v {
			return false
		}
	}
	return true
}

func (c *groupConsumer) AddGroupPartition(partition int32) {
	select {
	case c.addPartition <- partition:
	case <-c.stop:
	}
}

func (c *groupConsumer) waitForPartitions() bool {
	defer c.ensureEmpty()

	// if all registered, start consuming
	if c.partitionsRegistered() {
		return true
	}

	for {
		select {
		case par := <-c.addPartition:
			c.partitionMap[par] = true

			// if all registered, start consuming
			if c.partitionsRegistered() {
				return true
			}

		case <-c.stop:
			return false
		}
	}
}

func (c *groupConsumer) ensureEmpty() {
	for {
		select {
		case <-c.addPartition:
		default:
			return
		}
	}
}

func (c *groupConsumer) waitForMessages() bool {
	for {
		select {
		case n := <-c.consumer.Notifications():
			return c.handleNotification(n)

		case msg := <-c.consumer.Messages():
			select {
			case c.events <- &Message{
				Topic:     msg.Topic,
				Partition: msg.Partition,
				Offset:    msg.Offset,
				Timestamp: msg.Timestamp,
				Key:       string(msg.Key),
				Value:     msg.Value,
			}:
			case <-c.stop:
				return false
			}

		case err := <-c.consumer.Errors():
			select {
			case c.events <- &Error{err}:
			case <-c.stop:
				return false
			}

		case <-c.stop:
			return false
		}
	}
}

func (c *groupConsumer) run() {
	defer close(c.done)

	if !c.waitForNotification() {
		return
	}

	for {
		if !c.waitForPartitions() {
			return
		}

		if !c.waitForMessages() {
			return
		}
	}
}

func (c *groupConsumer) Commit(topic string, partition int32, offset int64) error {
	c.consumer.MarkPartitionOffset(topic, partition, offset, "")
	return nil
}

//go:generate mockgen -package mock -destination=mock/cluster_consumer.go -source=group_consumer.go clusterConsumer
type clusterConsumer interface {
	Close() error
	MarkPartitionOffset(topic string, partition int32, offset int64, metadata string)

	Notifications() <-chan *cluster.Notification
	Messages() <-chan *sarama.ConsumerMessage
	Errors() <-chan error
}
