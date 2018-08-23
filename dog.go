package es_writer

import (
	"go1/es-writer/action"
	"context"
	"github.com/streadway/amqp"
	"time"
	"gopkg.in/olivere/elastic.v5"
	"fmt"
	"github.com/sirupsen/logrus"
	"strings"
)

type Dog struct {
	debug bool

	// RabbitMQ
	ch      *amqp.Channel
	actions *action.Container
	count   int

	// ElasticSearch
	es   *elastic.Client
	bulk *elastic.BulkProcessor
}

func NewDog(ch *amqp.Channel, count int, es *elastic.Client, bulk *elastic.BulkProcessor, debug bool) *Dog {
	return &Dog{
		debug:   debug,
		ch:      ch,
		actions: action.NewContainer(),
		count:   count,
		es:      es,
		bulk:    bulk,
	}
}

func (w *Dog) UnitWorks() int {
	return w.actions.Length()
}

func (w *Dog) Start(ctx context.Context, flags Flags) (error) {
	ticker := time.NewTicker(*flags.TickInterval)
	messages, err := w.messages(flags)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ticker.C:
			if w.actions.Length() > 0 {
				w.flush()
			}

		case m := <-messages:
			if m.DeliveryTag == 0 {
				w.ch.Nack(m.DeliveryTag, false, false)
				continue
			}

			err := w.woof(ctx, m)
			if err != nil {
				logrus.WithError(err).Errorln("Failed to handle new message: " + string(m.Body))
			}
		}
	}

	return nil
}

func (w *Dog) messages(flags Flags) (<-chan amqp.Delivery, error) {
	queue, err := w.ch.QueueDeclare(*flags.QueueName, false, false, false, false, nil, )
	if nil != err {
		return nil, err
	}

	err = w.ch.QueueBind(queue.Name, *flags.RoutingKey, *flags.Exchange, true, nil)
	if nil != err {
		return nil, err
	}

	return w.ch.Consume(queue.Name, *flags.ConsumerName, false, false, false, true, nil)
}

func (w *Dog) woof(ctx context.Context, m amqp.Delivery) error {
	element, err := action.NewElement(m.DeliveryTag, m.Body)
	if err != nil {
		return err
	}

	// Not all requests are bulkable
	requestType := element.RequestType()
	if "bulkable" != requestType {
		if w.actions.Length() > 0 {
			w.flush()

			// TODO: We need more reasonal value than 2s.
			// We may have issue if make another request instantly after a bulk-request processing
			time.Sleep(2 * time.Second)
		}

		err = w.woooof(ctx, requestType, element)
		if err == nil {
			w.ch.Ack(m.DeliveryTag, true)
		}

		return err
	}

	if w.debug {
		logrus.Debugln("[woof] bulkable action: ", w.actions.Length()+1)
	}

	w.actions.Add(element)
	if w.actions.Length() >= w.count {
		w.flush()
	}

	return nil
}

func (w *Dog) woooof(ctx context.Context, requestType string, element action.Element) error {
	switch requestType {
	case "update_by_query":
		service, err := element.UpdateByQueryService(w.es)
		if err != nil {
			return err
		}

		conflictRetryIntervals := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 7 * time.Second, 0}
		for _, conflictRetryInterval := range conflictRetryIntervals {
			_, err = service.Do(ctx)
			if err == nil {
				break
			}

			if strings.Contains(err.Error(), "Error 409 (Conflict)") {
				logrus.WithError(err).Errorf("writing has conflict; try again in %s.\n", conflictRetryInterval)
				time.Sleep(conflictRetryInterval)
			}
		}

		return err

	case "delete_by_query":
		service, err := element.DeleteByQueryService(w.es)
		if err != nil {
			return err
		}

		conflictRetryIntervals := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 7 * time.Second, 0}
		for _, conflictRetryInterval := range conflictRetryIntervals {
			_, err = service.Do(ctx)
			if err == nil {
				break
			}

			if strings.Contains(err.Error(), "Error 409 (Conflict)") {
				logrus.WithError(err).Errorf("deleting has conflict; try again in %s.\n", conflictRetryInterval)
				time.Sleep(conflictRetryInterval)
			}
		}

		return err

	case "indices_create":
		service, err := element.IndicesCreateService(w.es)
		if err != nil {
			return err
		}

		_, err = service.Do(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				logrus.WithError(err).Errorln("That's ok if the index is existing.")
				return nil
			}
		}

		return err

	case "indices_delete":
		service, err := element.IndicesDeleteService(w.es)
		if err != nil {
			return err
		}

		_, err = service.Do(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "[type=index_not_found_exception]") {
				logrus.WithError(err).Infoln("That's ok if the index is not existing, already deleted somewhere.")
				return nil
			}
		}

		return err

	default:
		return fmt.Errorf("unsupported request type: %s", requestType)
	}

	return nil
}

func (w *Dog) flush() {
	deliveryTags := []uint64{}
	for _, element := range w.actions.Elements() {
		deliveryTags = append(deliveryTags, element.DeliveryTag)
		w.bulk.Add(element)
	}

	err := w.bulk.Flush()
	if err != nil {
		logrus.WithError(err).Errorln("failed flushing")
	}

	if err == nil {
		for _, deliveryTag := range deliveryTags {
			w.ch.Ack(deliveryTag, true)
		}

		w.actions.Clear()
	} else {
		// WHAT IF FAILED?
	}
}
