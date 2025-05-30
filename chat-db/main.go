package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"slices"
	"time"

	"github.com/IBM/sarama"
	"github.com/WadeCappa/real_time_chat/chat-db/chat_db"
	"github.com/WadeCappa/real_time_chat/chat-db/store"
	"github.com/WadeCappa/real_time_chat/chat-kafka-manager/consumer"
	"github.com/WadeCappa/real_time_chat/chat-kafka-manager/events"
	"github.com/gocql/gocql"
	"google.golang.org/grpc"
)

const (
	DEFAULT_KAFKA_HOSTNAME     = "localhost:9092"
	DEFAULT_CASSANDRA_HOSTNAME = "localhost:9042"
	DEFAULT_PORT               = 50054
	DEFAULT_LOAD_BATCH_SIZE    = 3
	TESTING_CHANNEL_ID         = 211
)

var (
	kafkaHostname     = flag.String("kafka-hostname", DEFAULT_KAFKA_HOSTNAME, "the hostname for kafka")
	cassandraHostname = flag.String("cassandra-hostname", DEFAULT_CASSANDRA_HOSTNAME, "the hostname for kafka")
	port              = flag.Int("port", DEFAULT_PORT, "port for this service")
)

type chatDbServer struct {
	chat_db.ChatdbServer
}

type message struct {
	channelId   int64
	userId      int64
	offset      int64
	time_posted time.Time
	content     string
}

type updateDataVisitor struct {
	events.EventVisitor

	metadata consumer.Metadata
}

func (v *updateDataVisitor) VisitNewChatMessageEvent(e events.NewChatMessageEvent) error {
	_, err := store.Call(*cassandraHostname, func(s *gocql.Session) (*bool, error) {
		err := s.Query(
			"insert into posts_db.messages (userId, offset, channelId, time_posted, content) values (?, ?, ?, ?, ?)",
			e.UserId,
			v.metadata.Offset,
			e.ChannelId,
			v.metadata.TimePosted,
			e.Content).WithContext(context.Background()).Exec()
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("failed to store new chat message event: %v", err)
	}
	return nil
}

func (s *chatDbServer) ReadMostRecent(request *chat_db.ReadMostRecentRequest, server grpc.ServerStreamingServer[chat_db.ReadMostRecentResponse]) error {

	messages, err := store.Call(*cassandraHostname, func(s *gocql.Session) (*[]*chat_db.ReadMostRecentResponse, error) {
		scanner := s.Query(
			"select userId, offset, channelId, time_posted, content from posts_db.messages where channelId = ? limit ?",
			request.ChannelId,
			DEFAULT_LOAD_BATCH_SIZE).WithContext(context.Background()).Iter().Scanner()

		messages := make([]*chat_db.ReadMostRecentResponse, 0)
		for scanner.Next() {
			var message message
			err := scanner.Scan(&message.userId, &message.offset, &message.channelId, &message.time_posted, &message.content)
			if err != nil {
				return nil, fmt.Errorf("failed to run scanner %v", err)
			}
			messages = append(messages, &chat_db.ReadMostRecentResponse{
				Message:            message.content,
				UserId:             message.userId,
				ChannelId:          message.channelId,
				Offset:             message.offset,
				TimePostedUnixTime: message.time_posted.Unix(),
			})
		}
		// scanner.Err() closes the iterator, so scanner nor iter should be used afterwards.
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("some error happened while reading from db %v", err)
		}

		slices.Reverse(messages)
		return &messages, nil
	})

	if err != nil {
		return fmt.Errorf("failed to load messages %v", err)
	}

	for _, message := range *messages {
		err = server.Send(message)
		if err != nil {
			return fmt.Errorf("failed to send message %v", err)
		}
	}

	return nil
}

// we can introduce batching here too to further decrease db load
func listenAndWrite(channelId, offset int64, kafkaUrl string, result chan error) {
	err := consumer.WatchChannel([]string{kafkaUrl}, channelId, offset, func(e events.Event, m consumer.Metadata) error {
		v := updateDataVisitor{metadata: m}
		err := e.Visit(&v)
		if err != nil {
			return fmt.Errorf("failed to visit data event %v", err)
		}
		log.Printf("successfully wrote message at offset %d", m.Offset)
		return nil
	})

	result <- err
}

func main() {
	flag.Parse()
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	res := make(chan error)

	lastOffset, err := store.Call(*cassandraHostname, func(s *gocql.Session) (*int64, error) {
		var lastOffset int64 = sarama.OffsetNewest
		err := s.Query("select max(offset) from posts_db.messages where channelid = ?",
			211).WithContext(context.Background()).Consistency(gocql.One).Scan(&lastOffset)
		return &lastOffset, err
	})
	if err != nil {
		log.Printf("failed to find latest offset: %v", err)
	}

	go listenAndWrite(TESTING_CHANNEL_ID, *lastOffset, *kafkaHostname, res)

	go func() {
		err := <-res
		if err != nil {
			log.Fatalf("failed to write to db: %v", err)
		}
	}()

	s := grpc.NewServer()
	chat_db.RegisterChatdbServer(s, &chatDbServer{})
	log.Printf("server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
