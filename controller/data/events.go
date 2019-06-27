package data

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/jackc/pgx"
)

type EventRepo struct {
	db *postgres.DB
}

func NewEventRepo(db *postgres.DB) *EventRepo {
	return &EventRepo{db: db}
}

type ListEventsOption func(opts *listEventsOptions)

type listEventsOptions struct {
	AppIDs       []string
	ObjectTypes  []string
	ObjectIDs    []string
	ObjectFields map[string][]string
	BeforeID     *int64
	SinceID      *int64
	Count        int
}

func ListEventsOptionAppIDs(appIDs []string) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.AppIDs = appIDs
	}
}

func ListEventsOptionObjectTypes(objectTypes []string) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.ObjectTypes = objectTypes
	}
}

func ListEventsOptionObjectIDs(objectIDs []string) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.ObjectIDs = objectIDs
	}
}

func ListEventsOptionObjectField(name string, values []string) ListEventsOption {
	return func(opts *listEventsOptions) {
		if opts.ObjectFields == nil {
			opts.ObjectFields = make(map[string][]string, 1)
		}
		f := opts.ObjectFields[name]
		f = append(f, values...)
		opts.ObjectFields[name] = f
	}
}

func ListEventsOptionBeforeID(beforeID int64) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.BeforeID = &beforeID
	}
}

func ListEventsOptionSinceID(sinceID int64) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.SinceID = &sinceID
	}
}

func ListEventsOptionCount(count int) ListEventsOption {
	return func(opts *listEventsOptions) {
		opts.Count = count
	}
}

func (r *EventRepo) LegacyListEvents(appIDs []string, objectTypes []string, objectIDs []string, beforeID *int64, sinceID *int64, count int) ([]*ct.Event, error) {
	options := make([]ListEventsOption, 0, 6)
	options = append(options, ListEventsOptionAppIDs(appIDs))
	options = append(options, ListEventsOptionObjectTypes(objectTypes))
	options = append(options, ListEventsOptionObjectIDs(objectIDs))
	if beforeID != nil {
		options = append(options, ListEventsOptionBeforeID(*beforeID))
	}
	if sinceID != nil {
		options = append(options, ListEventsOptionSinceID(*sinceID))
	}
	options = append(options, ListEventsOptionCount(count))
	return r.ListEvents(options...)
}

func (r *EventRepo) ListEvents(options ...ListEventsOption) ([]*ct.Event, error) {
	opts := &listEventsOptions{}
	for _, fn := range options {
		fn(opts)
	}

	query := "SELECT event_id, app_id, object_id, object_type, data, op, created_at FROM events"
	var conditions [][]string
	var n int
	args := []interface{}{}
	if opts.BeforeID != nil {
		n++
		conditions = append(conditions, []string{fmt.Sprintf("event_id < $%d", n)})
		args = append(args, *opts.BeforeID)
	}
	if opts.SinceID != nil {
		n++
		conditions = append(conditions, []string{fmt.Sprintf("event_id > $%d", n)})
		args = append(args, *opts.SinceID)
	}
	if len(opts.AppIDs) > 0 || len(opts.ObjectIDs) > 0 || len(opts.ObjectFields) > 0 {
		c := []string{}
		if len(opts.AppIDs) > 0 {
			n++
			c = append(c, fmt.Sprintf("app_id::text = ANY($%d::text[])", n))
			args = append(args, opts.AppIDs)
		}
		if len(opts.ObjectIDs) > 0 {
			n++
			c = append(c, fmt.Sprintf("object_id::text = ANY($%d::text[])", n))
			args = append(args, opts.ObjectIDs)
		}
		if len(opts.ObjectFields) > 0 {
			for name, values := range opts.ObjectFields {
				for _, value := range values {
					c = append(c, fmt.Sprintf("data->>$%d = $%d", n+1, n+2))
					n = n + 2
					args = append(args, name, value)
				}
			}
		}
		if len(c) > 0 {
			conditions = append(conditions, c)
		}
	}
	if len(opts.ObjectTypes) > 0 {
		c := make([]string, 0, len(opts.ObjectTypes))
		for _, typ := range opts.ObjectTypes {
			n++
			c = append(c, fmt.Sprintf("object_type = $%d", n))
			args = append(args, typ)
		}
		conditions = append(conditions, c)
	}
	if len(conditions) > 0 {
		query += " WHERE "
		for i, c := range conditions {
			if i > 0 {
				query += " AND "
			}
			query += fmt.Sprintf("(%s)", strings.Join(c, " OR "))
		}
	}
	query += " ORDER BY event_id DESC"
	if opts.Count > 0 {
		n++
		query += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, opts.Count)
	}
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var events []*ct.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (r *EventRepo) GetEvent(id int64) (*ct.Event, error) {
	row := r.db.QueryRow("event_select", id)
	return scanEvent(row)
}

func scanEvent(s postgres.Scanner) (*ct.Event, error) {
	var event ct.Event
	var typ string
	var data []byte
	var appID *string
	var op *string
	err := s.Scan(&event.ID, &appID, &event.ObjectID, &typ, &data, &op, &event.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			err = ErrNotFound
		}
		return nil, err
	}
	if appID != nil {
		event.AppID = *appID
	}
	if data == nil {
		data = []byte("null")
	}
	if op != nil {
		event.Op = ct.EventOp(*op)
	}
	event.ObjectType = ct.EventType(typ)
	event.Data = json.RawMessage(data)
	return &event, nil
}

// ErrEventBufferOverflow is returned to clients when the in-memory event
// buffer is full due to clients not reading events quickly enough.
var ErrEventBufferOverflow = errors.New("event stream buffer overflow")

// eventBufferSize is the amount of events to buffer in memory.
const eventBufferSize = 1000

// EventSubscriber receives events from the EventListener loop and maintains
// it's own loop to forward those events to the Events channel.
type EventSubscriber struct {
	Events  chan *ct.Event
	Err     error
	errOnce sync.Once

	l           *EventListener
	queue       chan *ct.Event
	appID       string
	objectTypes []string
	objectIDs   []string

	stop     chan struct{}
	stopOnce sync.Once
}

// Notify filters the event based on it's type and objectID and then pushes
// it to the event queue.
func (e *EventSubscriber) Notify(event *ct.Event) {
	if len(e.objectTypes) > 0 {
		foundType := false
		for _, typ := range e.objectTypes {
			if typ == string(event.ObjectType) {
				foundType = true
				break
			}
		}
		if !foundType {
			return
		}
	}
	if len(e.objectIDs) > 0 {
		foundID := false
		for _, id := range e.objectIDs {
			if id == event.ObjectID {
				foundID = true
				break
			}
		}
		if !foundID {
			return
		}
	}
	select {
	case e.queue <- event:
	default:
		// Run in a goroutine to avoid deadlock with Notify
		go e.CloseWithError(ErrEventBufferOverflow)
	}
}

// loop pops events off the queue and sends them to the Events channel.
func (e *EventSubscriber) loop() {
	defer close(e.Events)
	for {
		select {
		case <-e.stop:
			return
		case event := <-e.queue:
			e.Events <- event
		}
	}
}

// Close unsubscribes from the EventListener and stops the loop.
func (e *EventSubscriber) Close() {
	e.l.Unsubscribe(e)
	e.stopOnce.Do(func() { close(e.stop) })
}

// CloseWithError sets the Err field and then closes the subscriber.
func (e *EventSubscriber) CloseWithError(err error) {
	e.errOnce.Do(func() { e.Err = err })
	e.Close()
}

func NewEventListener(r *EventRepo) *EventListener {
	return &EventListener{
		eventRepo:   r,
		subscribers: make(map[string]map[*EventSubscriber]struct{}),
		doneCh:      make(chan struct{}),
	}
}

// EventListener creates a postgres Listener for events and forwards them
// to subscribers.
type EventListener struct {
	eventRepo *EventRepo

	subscribers map[string]map[*EventSubscriber]struct{}
	subMtx      sync.RWMutex

	closed    bool
	closedMtx sync.RWMutex
	doneCh    chan struct{}
}

// Subscribe creates and returns an EventSubscriber for the given app, type and object.
// Using an empty string for appID subscribes to all apps
func (e *EventListener) Subscribe(appID string, objectTypes []string, objectIDs []string) (*EventSubscriber, error) {
	e.subMtx.Lock()
	defer e.subMtx.Unlock()
	if e.IsClosed() {
		return nil, errors.New("event listener closed")
	}
	s := &EventSubscriber{
		Events:      make(chan *ct.Event),
		l:           e,
		queue:       make(chan *ct.Event, eventBufferSize),
		stop:        make(chan struct{}),
		appID:       appID,
		objectTypes: objectTypes,
		objectIDs:   objectIDs,
	}
	go s.loop()
	if _, ok := e.subscribers[appID]; !ok {
		e.subscribers[appID] = make(map[*EventSubscriber]struct{})
	}
	e.subscribers[appID][s] = struct{}{}
	return s, nil
}

// Unsubscribe unsubscribes the given subscriber.
func (e *EventListener) Unsubscribe(s *EventSubscriber) {
	e.subMtx.Lock()
	defer e.subMtx.Unlock()
	if subs, ok := e.subscribers[s.appID]; ok {
		delete(subs, s)
		if len(subs) == 0 {
			delete(e.subscribers, s.appID)
		}
	}
}

// Listen creates a postgres listener for events and starts a goroutine to
// forward the events to subscribers.
func (e *EventListener) Listen() error {
	log := logger.New("fn", "EventListener.Listen")
	listener, err := e.eventRepo.db.Listen("events", log)
	if err != nil {
		e.CloseWithError(err)
		return err
	}
	go func() {
		for {
			select {
			case n, ok := <-listener.Notify:
				if !ok {
					e.CloseWithError(listener.Err)
					return
				}
				idApp := strings.SplitN(n.Payload, ":", 2)
				if len(idApp) < 1 {
					log.Error(fmt.Sprintf("invalid event notification: %q", n.Payload))
					continue
				}
				id, err := strconv.ParseInt(idApp[0], 10, 64)
				if err != nil {
					log.Error(fmt.Sprintf("invalid event notification: %q", n.Payload), "err", err)
					continue
				}
				event, err := e.eventRepo.GetEvent(id)
				if err != nil {
					log.Error(fmt.Sprintf("invalid event notification: %q", n.Payload), "err", err)
					continue
				}
				e.Notify(event)
			case <-e.doneCh:
				listener.Close()
				return
			}
		}
	}()
	return nil
}

// Notify notifies all sbscribers of the given event.
func (e *EventListener) Notify(event *ct.Event) {
	e.subMtx.RLock()
	defer e.subMtx.RUnlock()
	if subs, ok := e.subscribers[event.AppID]; ok {
		for sub := range subs {
			sub.Notify(event)
		}
	}
	if event.AppID != "" {
		// Ensure subscribers not filtering by app get the event
		if subs, ok := e.subscribers[""]; ok {
			for sub := range subs {
				sub.Notify(event)
			}
		}
	}
}

// IsClosed returns whether or not the listener is closed.
func (e *EventListener) IsClosed() bool {
	e.closedMtx.RLock()
	defer e.closedMtx.RUnlock()
	return e.closed
}

// CloseWithError marks the listener as closed and closes all subscribers
// with the given error.
func (e *EventListener) CloseWithError(err error) {
	e.closedMtx.Lock()
	if e.closed {
		e.closedMtx.Unlock()
		return
	}
	e.closed = true
	e.closedMtx.Unlock()

	e.subMtx.RLock()
	defer e.subMtx.RUnlock()
	subscribers := e.subscribers
	for _, subs := range subscribers {
		for sub := range subs {
			if err == nil {
				go sub.Close()
			} else {
				go sub.CloseWithError(err)
			}
		}
	}
	close(e.doneCh)
}
