package consumer

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/log"
)

var testMsg = sarama.StringEncoder("Foo")

// If a particular offset is provided then messages are consumed starting from
// that offset.
func TestConsumerOffsetManual(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)

	mockFetchResponse := sarama.NewMockFetchResponse(t, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i+1234), testMsg)
	}

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 2345),
		"FetchRequest": mockFetchResponse,
	})

	// When
	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	consumer, concreteOffset, err := master.ConsumePartition("my_topic", 0, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if concreteOffset != 1234 {
		t.Fatalf("Invalid cocrete offset: want=10, got=%d", concreteOffset)
	}

	// Then: messages starting from offset 1234 are consumed.
	for i := 0; i < 10; i++ {
		select {
		case message := <-consumer.Messages():
			assertMessageOffset(t, message, int64(i+1234))
		case err := <-consumer.Errors():
			t.Error(err)
		}
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// If `sarama.OffsetNewest` is passed as the initial offset then the first consumed
// message is indeed corresponds to the offset that broker claims to be the
// newest in its metadata response.
func TestConsumerOffsetNewest(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 10).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 7),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 9, testMsg).
			SetMessage("my_topic", 0, 10, testMsg).
			SetMessage("my_topic", 0, 11, testMsg).
			SetHighWaterMark("my_topic", 0, 14),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, concreteOffset, err := master.ConsumePartition("my_topic", 0, sarama.OffsetNewest)
	if err != nil {
		t.Fatal(err)
	}
	if concreteOffset != 10 {
		t.Fatalf("Invalid cocrete offset: want=10, got=%d", concreteOffset)
	}

	// Then
	msg := <-consumer.Messages()
	assertMessageOffset(t, msg, 10)
	if msg.HighWaterMark != 14 {
		t.Errorf("Invalid high water mark: expected=14, actual=%d", msg.HighWaterMark)
	}
	if hwmo := consumer.HighWaterMarkOffset(); hwmo != 14 {
		t.Errorf("Expected high water mark offset 14, found %d", hwmo)
	}

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// It is possible to close a partition consumer and create the same anew.
func TestConsumerRecreate(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 10, testMsg),
	})

	c, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	pc, _, err := c.ConsumePartition("my_topic", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertMessageOffset(t, <-pc.Messages(), 10)

	// When
	safeClose(t, pc)
	pc, _, err = c.ConsumePartition("my_topic", 0, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Then
	assertMessageOffset(t, <-pc.Messages(), 10)

	safeClose(t, pc)
	safeClose(t, c)
	broker0.Close()
}

// An attempt to consume the same partition twice should fail.
func TestConsumerDuplicate(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1),
	})

	config := sarama.NewConfig()
	config.ChannelBufferSize = 0
	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc1, _, err := c.ConsumePartition("my_topic", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// When
	pc2, _, err := c.ConsumePartition("my_topic", 0, 0)

	// Then
	if pc2 != nil || err != sarama.ConfigurationError("That topic/partition is already being consumed") {
		t.Fatal("A partition cannot be consumed twice at the same time")
	}

	safeClose(t, pc1)
	safeClose(t, c)
	broker0.Close()
}

// If consumer fails to refresh metadata it keeps retrying with frequency
// specified by `Config.Consumer.Retry.Backoff`.
func TestConsumerLeaderRefreshError(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 100)

	// Stage 1: my_topic/0 served by broker0
	log.Infof("    STAGE 1")

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 123).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 200 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0
	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc, _, err := c.ConsumePartition("my_topic", 0, sarama.OffsetOldest)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-pc.Messages(), 123)

	// Stage 2: broker0 says that it is no longer the leader for my_topic/0,
	// but the requests to retrieve metadata fail with network timeout.
	log.Infof("    STAGE 2")

	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse2),
	})

	if consErr := <-pc.Errors(); consErr.Err != sarama.ErrNotLeaderForPartition {
		t.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Stage 3: finally the metadata returned by broker0 tells that broker1 is
	// a new leader for my_topic/0. Consumption resumes.

	log.Infof("    STAGE 3")

	broker1 := sarama.NewMockBroker(t, 101)

	broker1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 124, testMsg),
	})
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(broker1.Addr(), broker1.BrokerID()).
			SetLeader("my_topic", 0, broker1.BrokerID()),
	})

	assertMessageOffset(t, <-pc.Messages(), 124)

	safeClose(t, pc)
	safeClose(t, c)
	broker1.Close()
	broker0.Close()
}

func TestConsumerInvalidTopic(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 100)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()),
	})

	c, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When
	pc, _, err := c.ConsumePartition("my_topic", 0, sarama.OffsetOldest)

	// Then
	if pc != nil || err != sarama.ErrUnknownTopicOrPartition {
		t.Errorf("Should fail with, err=%v", err)
	}

	safeClose(t, c)
	broker0.Close()
}

// Nothing bad happens if a partition consumer that has no leader assigned at
// the moment is closed.
func TestConsumerClosePartitionWithoutLeader(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 100)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 123).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0
	c, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	pc, _, err := c.ConsumePartition("my_topic", 0, sarama.OffsetOldest)
	if err != nil {
		t.Fatal(err)
	}

	assertMessageOffset(t, <-pc.Messages(), 123)

	// broker0 says that it is no longer the leader for my_topic/0, but the
	// requests to retrieve metadata fail with network timeout.
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse2),
	})

	// When
	if consErr := <-pc.Errors(); consErr.Err != sarama.ErrNotLeaderForPartition {
		t.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Then: the partition consumer can be closed without any problem.
	safeClose(t, pc)
	safeClose(t, c)
	broker0.Close()
}

// If the initial offset passed on partition consumer creation is out of the
// actual offset range for the partition, then the partition consumer stops
// immediately closing its output channels.
func TestConsumerShutsDownOutOfRange(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	fetchResponse := new(sarama.FetchResponse)
	fetchResponse.AddError("my_topic", 0, sarama.ErrOffsetOutOfRange)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 7),
		"FetchRequest": sarama.NewMockWrapper(fetchResponse),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, _, err := master.ConsumePartition("my_topic", 0, 101)
	if err != nil {
		t.Fatal(err)
	}

	// Then: consumer should shut down closing its messages and errors channels.
	if _, ok := <-consumer.Messages(); ok {
		t.Error("Expected the consumer to shut down")
	}
	safeClose(t, consumer)

	safeClose(t, master)
	broker0.Close()
}

// If a fetch response contains messages with offsets that are smaller then
// requested, then such messages are ignored.
func TestConsumerExtraOffsets(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	fetchResponse1 := &sarama.FetchResponse{}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 4)
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNoError)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0),
		"FetchRequest": sarama.NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, _, err := master.ConsumePartition("my_topic", 0, 3)
	if err != nil {
		t.Fatal(err)
	}

	// Then: messages with offsets 1 and 2 are not returned even though they
	// are present in the response.
	assertMessageOffset(t, <-consumer.Messages(), 3)
	assertMessageOffset(t, <-consumer.Messages(), 4)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// It is fine if offsets of fetched messages are not sequential (although
// strictly increasing!).
func TestConsumerNonSequentialOffsets(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	fetchResponse1 := &sarama.FetchResponse{}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 5)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 7)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 11)
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNoError)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0),
		"FetchRequest": sarama.NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When
	consumer, _, err := master.ConsumePartition("my_topic", 0, 3)
	if err != nil {
		t.Fatal(err)
	}

	// Then: messages with offsets 1 and 2 are not returned even though they
	// are present in the response.
	assertMessageOffset(t, <-consumer.Messages(), 5)
	assertMessageOffset(t, <-consumer.Messages(), 7)
	assertMessageOffset(t, <-consumer.Messages(), 11)

	safeClose(t, consumer)
	safeClose(t, master)
	broker0.Close()
}

// If leadership for a partition is changing then consumer resolves the new
// leader and switches to it.
func TestConsumerRebalancingMultiplePartitions(t *testing.T) {
	// initial setup
	seedBroker := sarama.NewMockBroker(t, 10)
	leader0 := sarama.NewMockBroker(t, 0)
	leader1 := sarama.NewMockBroker(t, 1)

	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(leader0.Addr(), leader0.BrokerID()).
			SetBroker(leader1.Addr(), leader1.BrokerID()).
			SetLeader("my_topic", 0, leader0.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()),
	})

	mockOffsetResponse1 := sarama.NewMockOffsetResponse(t).
		SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
		SetOffset("my_topic", 0, sarama.OffsetNewest, 1000).
		SetOffset("my_topic", 1, sarama.OffsetOldest, 0).
		SetOffset("my_topic", 1, sarama.OffsetNewest, 1000)
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  sarama.NewMockFetchResponse(t, 1),
	})
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  sarama.NewMockFetchResponse(t, 1),
	})

	// launch test goroutines
	config := sarama.NewConfig()
	config.Consumer.Retry.Backoff = 50 * time.Millisecond
	master, err := NewConsumer([]string{seedBroker.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	// we expect to end up (eventually) consuming exactly ten messages on each partition
	var wg sync.WaitGroup
	for i := int32(0); i < 2; i++ {
		consumer, _, err := master.ConsumePartition("my_topic", i, 0)
		if err != nil {
			t.Error(err)
		}

		go func(c PartitionConsumer) {
			for err := range c.Errors() {
				t.Error(err)
			}
		}(consumer)

		wg.Add(1)
		go func(partition int32, c PartitionConsumer) {
			for i := 0; i < 10; i++ {
				message := <-consumer.Messages()
				if message.Offset != int64(i) {
					t.Error("Incorrect message offset!", i, partition, message.Offset)
				}
				if message.Partition != partition {
					t.Error("Incorrect message partition!")
				}
			}
			safeClose(t, consumer)
			wg.Done()
		}(i, consumer)
	}

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 1")
	// Stage 1:
	//   * my_topic/0 -> leader0 serves 4 messages
	//   * my_topic/1 -> leader1 serves 0 messages

	mockFetchResponse := sarama.NewMockFetchResponse(t, 1)
	for i := 0; i < 4; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 2")
	// Stage 2:
	//   * leader0 says that it is no longer serving my_topic/0
	//   * seedBroker tells that leader1 is serving my_topic/0 now

	// seed broker tells that the new partition 0 leader is leader1
	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()),
	})

	// leader0 says no longer leader of partition 0
	fetchResponse := new(sarama.FetchResponse)
	fetchResponse.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse),
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 3")
	// Stage 3:
	//   * my_topic/0 -> leader1 serves 6 messages
	//   * my_topic/1 -> leader1 server 8 messages

	// leader1 provides 3 message on partition 0, and 8 messages on partition 1
	mockFetchResponse2 := sarama.NewMockFetchResponse(t, 2)
	for i := 4; i < 10; i++ {
		mockFetchResponse2.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	for i := 0; i < 8; i++ {
		mockFetchResponse2.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse2,
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 4")
	// Stage 4:
	//   * my_topic/1 -> leader1 tells that it is no longer the leader
	//   * seedBroker tells that leader0 is a new leader for my_topic/1

	// metadata assigns 0 to leader1 and 1 to leader0
	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader0.BrokerID()),
	})

	// leader1 says no longer leader of partition1
	fetchResponse4 := new(sarama.FetchResponse)
	fetchResponse4.AddError("my_topic", 1, sarama.ErrNotLeaderForPartition)
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse4),
	})

	// leader0 provides two messages on partition 1
	mockFetchResponse4 := sarama.NewMockFetchResponse(t, 2)
	for i := 8; i < 10; i++ {
		mockFetchResponse4.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse4,
	})

	wg.Wait()
	safeClose(t, master)
	leader1.Close()
	leader0.Close()
	seedBroker.Close()
}

// When two partitions have the same broker as the leader, if one partition
// consumer channel buffer is full then that does not affect the ability to
// read messages by the other consumer.
func TestConsumerInterleavedClose(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()).
			SetLeader("my_topic", 1, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 1000).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1100).
			SetOffset("my_topic", 1, sarama.OffsetOldest, 2000).
			SetOffset("my_topic", 1, sarama.OffsetNewest, 2100),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage("my_topic", 0, 1000, testMsg).
			SetMessage("my_topic", 0, 1001, testMsg).
			SetMessage("my_topic", 0, 1002, testMsg).
			SetMessage("my_topic", 1, 2000, testMsg),
	})

	config := sarama.NewConfig()
	config.ChannelBufferSize = 0
	master, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	c0, _, err := master.ConsumePartition("my_topic", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	c1, _, err := master.ConsumePartition("my_topic", 1, 2000)
	if err != nil {
		t.Fatal(err)
	}

	// When/Then: we can read from partition 0 even if nobody reads from partition 1
	assertMessageOffset(t, <-c0.Messages(), 1000)
	assertMessageOffset(t, <-c0.Messages(), 1001)
	assertMessageOffset(t, <-c0.Messages(), 1002)

	safeClose(t, c1)
	safeClose(t, c0)
	safeClose(t, master)
	broker0.Close()
}

func TestConsumerBounceWithReferenceOpen(t *testing.T) {
	broker0 := sarama.NewMockBroker(t, 0)
	broker0Addr := broker0.Addr()
	broker1 := sarama.NewMockBroker(t, 1)

	mockMetadataResponse := sarama.NewMockMetadataResponse(t).
		SetBroker(broker0.Addr(), broker0.BrokerID()).
		SetBroker(broker1.Addr(), broker1.BrokerID()).
		SetLeader("my_topic", 0, broker0.BrokerID()).
		SetLeader("my_topic", 1, broker1.BrokerID())

	mockOffsetResponse := sarama.NewMockOffsetResponse(t).
		SetOffset("my_topic", 0, sarama.OffsetOldest, 1000).
		SetOffset("my_topic", 0, sarama.OffsetNewest, 1100).
		SetOffset("my_topic", 1, sarama.OffsetOldest, 2000).
		SetOffset("my_topic", 1, sarama.OffsetNewest, 2100)

	mockFetchResponse := sarama.NewMockFetchResponse(t, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(1000+i), testMsg)
		mockFetchResponse.SetMessage("my_topic", 1, int64(2000+i), testMsg)
	}

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse,
		"FetchRequest":  mockFetchResponse,
	})
	broker1.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": mockMetadataResponse,
		"OffsetRequest":   mockOffsetResponse,
		"FetchRequest":    mockFetchResponse,
	})

	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.ChannelBufferSize = 1
	master, err := NewConsumer([]string{broker1.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	c0, _, err := master.ConsumePartition("my_topic", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	c1, _, err := master.ConsumePartition("my_topic", 1, 2000)
	if err != nil {
		t.Fatal(err)
	}

	// read messages from both partition to make sure that both brokers operate
	// normally.
	assertMessageOffset(t, <-c0.Messages(), 1000)
	assertMessageOffset(t, <-c1.Messages(), 2000)

	// Simulate broker shutdown. Note that metadata response does not change,
	// that is the leadership does not move to another broker. So partition
	// consumer will keep retrying to restore the connection with the broker.
	broker0.Close()

	// Make sure that while the partition/0 leader is down, consumer/partition/1
	// is capable of pulling messages from broker1.
	for i := 1; i < 7; i++ {
		offset := (<-c1.Messages()).Offset
		if offset != int64(2000+i) {
			t.Errorf("Expected offset %d from consumer/partition/1", int64(2000+i))
		}
	}

	// Bring broker0 back to service.
	broker0 = sarama.NewMockBrokerAddr(t, 0, broker0Addr)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	// Read the rest of messages from both partitions.
	for i := 7; i < 10; i++ {
		assertMessageOffset(t, <-c1.Messages(), int64(2000+i))
	}
	for i := 1; i < 10; i++ {
		assertMessageOffset(t, <-c0.Messages(), int64(1000+i))
	}

	select {
	case <-c0.Errors():
	default:
		t.Errorf("Partition consumer should have detected broker restart")
	}

	safeClose(t, c1)
	safeClose(t, c0)
	safeClose(t, master)
	broker0.Close()
	broker1.Close()
}

func TestConsumerOffsetOutOfRange(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 2)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 2345),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// When/Then
	if _, _, err := master.ConsumePartition("my_topic", 0, 0); err != sarama.ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}
	if _, _, err := master.ConsumePartition("my_topic", 0, 3456); err != sarama.ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}
	if _, _, err := master.ConsumePartition("my_topic", 0, -3); err != sarama.ErrOffsetOutOfRange {
		t.Fatal("Should return ErrOffsetOutOfRange, got:", err)
	}

	safeClose(t, master)
	broker0.Close()
}

// When a master consumer is closed all kittens gets killed.
func TestConsumerClose(t *testing.T) {
	// Given
	broker0 := sarama.NewMockBroker(t, 0)
	defer broker0.Close()

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 100).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 1),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 500 * time.Millisecond
	master, err := NewConsumer([]string{broker0.Addr()}, config)
	if err != nil {
		t.Fatal(err)
	}

	// The mock broker is configured not to reply to FetchRequest's. That will
	// make some internal goroutine block for `Config.Net.ReadTimeout`.
	_, _, _ = master.ConsumePartition("my_topic", 0, sarama.OffsetNewest)

	// When/Then: close the consumer while an internal broker consumer is
	// waiting for a response.
	safeClose(t, master)
}

func assertMessageOffset(t *testing.T, msg *ConsumerMessage, expectedOffset int64) {
	if msg.Offset != expectedOffset {
		t.Errorf("Incorrect message offset: expected=%d, actual=%d", expectedOffset, msg.Offset)
	}
}

func safeClose(t testing.TB, c io.Closer) {
	err := c.Close()
	if err != nil {
		t.Error(err)
	}
}