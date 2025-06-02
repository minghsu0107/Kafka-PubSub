package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/message/router/plugin"
)

var (
	brokers      = []string{"kafka:9092"}
	consumeTopic = "events"
	publishTopic = "events-processed"

	logger = watermill.NewStdLogger(
		true,  // debug
		false, // trace
	)
	marshaler = kafka.DefaultMarshaler{}
)

type event struct {
	ID int `json:"id"`
}

type processedEvent struct {
	ProcessedID int       `json:"processed_id"`
	Time        time.Time `json:"time"`
}

func main() {
	publisher := createPublisher()
	saramaPublisher, err := createSaramaPublisher()
	if err != nil {
		panic(err)
	}

	// Subscriber is created with consumer group handler_1
	subscriber := createSubscriber("handler_1")

	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		panic(err)
	}

	router.AddPlugin(plugin.SignalsHandler)
	router.AddMiddleware(middleware.Recoverer)

	// Adding a handler (multiple handlers can be added)
	router.AddHandler(
		"handler_1",  // handler name, must be unique
		consumeTopic, // topic from which messages should be consumed
		subscriber,
		publishTopic, // topic to which messages should be published
		publisher,
		// the original message is fetched by Kafka client and send to watermill's channel
		// if handler returns error and exeeds max retries, the router marks the watermill's message as NACKed
		// -> resend to watermill's channel again (which is a unbuffered channel -> block when there is no msg reader)
		// otherwise, marks the watermill's message as ACKed
		// -> the original message is marked as consumed, committed offset += 1 and wil be auto-committed soon
		func(msg *message.Message) ([]*message.Message, error) {
			consumedPayload := event{}
			err := json.Unmarshal(msg.Payload, &consumedPayload)
			if err != nil {
				// When a handler returns an error, the default behavior is to send a Nack (negative-acknowledgement).
				// The message will be processed again.
				//
				// You can change the default behaviour by using middlewares, like Retry or PoisonQueue.
				// You can also implement your own middleware.
				return nil, err
			}

			log.Printf("received event %+v", consumedPayload)

			newPayload, err := json.Marshal(processedEvent{
				ProcessedID: consumedPayload.ID,
				Time:        time.Now(),
			})
			if err != nil {
				return nil, err
			}

			newMessage := message.NewMessage(watermill.NewUUID(), newPayload)

			return []*message.Message{newMessage}, nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup
	wg.Add(2)

	// Simulate incoming events in the background
	go simulateEvents(ctx, saramaPublisher, &wg)

	go func() {
		defer wg.Done()
		if err := router.Run(ctx); err != nil {
			panic(err)
		}
	}()

	<-sig
	cancel()

	wg.Wait()
}

// createPublisher is a helper function that creates a Publisher, in this case - the Kafka Publisher.
// Publish is blocking and wait for ack from Kafka.
func createPublisher() message.Publisher {
	kafkaPublisher, err := kafka.NewPublisher(
		kafka.PublisherConfig{
			Brokers:   brokers,
			Marshaler: marshaler,
			// OverwriteSaramaConfig: &sarama.Config{},
		},
		logger,
	)
	if err != nil {
		panic(err)
	}

	return kafkaPublisher
}

func createSaramaPublisher() (sarama.AsyncProducer, error) {
	config := sarama.NewConfig()
	saramaVersion, err := sarama.ParseKafkaVersion("3.6.0")
	if err != nil {
		panic(err)
	}
	config.Version = saramaVersion
	config.Producer.Partitioner = sarama.NewHashPartitioner
	config.Producer.Return.Successes = false
	config.Producer.Flush.Bytes = 3000000
	config.Producer.Flush.Messages = 50
	config.Producer.Flush.Frequency = 100 * time.Millisecond
	return sarama.NewAsyncProducer(brokers, config)
}

// createSubscriber is a helper function similar to the previous one, but in this case it creates a Subscriber.
func createSubscriber(consumerGroup string) message.Subscriber {
	config := sarama.NewConfig()
	saramaVersion, err := sarama.ParseKafkaVersion("3.6.0")
	if err != nil {
		panic(err)
	}
	config.Version = saramaVersion
	config.Consumer.Fetch.Default = 1024 * 1024
	config.Consumer.Offsets.AutoCommit.Enable = true
	config.Consumer.Offsets.AutoCommit.Interval = 1 * time.Second

	kafkaSubscriber, err := kafka.NewSubscriber(
		kafka.SubscriberConfig{
			Brokers:     brokers,
			Unmarshaler: marshaler,
			// When empty, if read offset from latest, then all messages sent after subscription from all partitions will be returned (process each partition in separate goroutines)
			// this way, we disregard consumer group and assign partitions to the consumer directly
			// assign mode does not affect offsets of other consumers and consumer groups
			ConsumerGroup: consumerGroup, // every handler will use a separate consumer group
			// Kafka automatically generates a consumer.id which is used by itself to identify the active consumers in a consumer group
			// so it is not possible to manually set the consumer.id for Kafka Consumers
			InitializeTopicDetails: &sarama.TopicDetail{
				NumPartitions:     2, // number of partitions
				ReplicationFactor: 2, // number of replications of each partition
			},
			OverwriteSaramaConfig: config,
		},
		logger,
	)
	if err != nil {
		panic(err)
	}

	return kafkaSubscriber
}

// simulateEvents produces events that will be later consumed.
func simulateEvents(ctx context.Context, publisher sarama.AsyncProducer, wg *sync.WaitGroup) {
	defer wg.Done()

	i := 0
	for {
		e := event{
			ID: i,
		}

		payload, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}

		saramaMsg, err := marshaler.Marshal(consumeTopic, message.NewMessage(
			watermill.NewUUID(), // internal uuid of the message, useful for debugging
			payload,
		))
		if err != nil {
			fmt.Println(err)
		}
		// saramaMsg.Key = sarama.ByteEncoder([]byte("mykey"))

		select {
		case publisher.Input() <- saramaMsg:
			i++
		case err := <-publisher.Errors():
			fmt.Println(err)
		case <-ctx.Done():
			// Close shuts down the producer and waits for any buffered messages to be flushed
			if err := publisher.Close(); err != nil {
				fmt.Println(err)
			}
			return
		}

		time.Sleep(time.Second)
	}
}
