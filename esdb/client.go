// Package esdb EventStoreDB gRPC client.
package esdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	persistentProto "github.com/EventStore/EventStore-Client-Go/v2/protos/persistent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	api "github.com/EventStore/EventStore-Client-Go/v2/protos/streams"
)

// Client Represents a client to a single node. A client instance maintains a full duplex communication to EventStoreDB.
// Many threads can use an EventStoreDB client at the same time or a single thread can make many asynchronous requests.
type Client struct {
	grpcClient *grpcClient
	config     *Configuration
}

// NewClient Creates a gRPC client to an EventStoreDB database.
func NewClient(configuration *Configuration) (*Client, error) {
	grpcClient := newGrpcClient(*configuration)
	return &Client{
		grpcClient: grpcClient,
		config:     configuration,
	}, nil
}

// Close Closes a connection and cleans all its allocated resources.
func (client *Client) Close() error {
	client.grpcClient.close()
	return nil
}

// AppendToStream Appends events to a given stream.
func (client *Client) AppendToStream(
	context context.Context,
	streamID string,
	opts AppendToStreamOptions,
	events ...EventData,
) (*WriteResult, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(context, client.config, &opts, callOptions)
	defer cancel()

	appendOperation, err := streamsClient.Append(ctx, callOptions...)
	if err != nil {
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("could not construct append operation. Reason: %w", err)
	}

	header := toAppendHeader(streamID, opts.ExpectedRevision)

	if err := appendOperation.Send(header); err != nil {
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("could not send append request header. Reason: %w", err)
	}

	for _, event := range events {
		appendRequest := &api.AppendReq{
			Content: &api.AppendReq_ProposedMessage_{
				ProposedMessage: toProposedMessage(event),
			},
		}

		if err = appendOperation.Send(appendRequest); err != nil {
			err = client.grpcClient.handleError(handle, headers, trailers, err)
			return nil, fmt.Errorf("could not send append request. Reason: %w", err)
		}
	}

	response, err := appendOperation.CloseAndRecv()
	if err != nil {
		return nil, client.grpcClient.handleError(handle, headers, trailers, err)
	}

	result := response.GetResult()
	switch result.(type) {
	case *api.AppendResp_Success_:
		{
			success := result.(*api.AppendResp_Success_)
			var streamRevision uint64
			if _, ok := success.Success.GetCurrentRevisionOption().(*api.AppendResp_Success_NoStream); ok {
				streamRevision = 1
			} else {
				streamRevision = success.Success.GetCurrentRevision()
			}

			var commitPosition uint64
			var preparePosition uint64
			if position, ok := success.Success.GetPositionOption().(*api.AppendResp_Success_Position); ok {
				commitPosition = position.Position.CommitPosition
				preparePosition = position.Position.PreparePosition
			} else {
				streamRevision = success.Success.GetCurrentRevision()
			}

			return &WriteResult{
				CommitPosition:      commitPosition,
				PreparePosition:     preparePosition,
				NextExpectedVersion: streamRevision,
			}, nil
		}
	case *api.AppendResp_WrongExpectedVersion_:
		{
			wrong := result.(*api.AppendResp_WrongExpectedVersion_).WrongExpectedVersion
			expected := ""
			current := ""

			if wrong.GetExpectedAny() != nil {
				expected = "any"
			} else if wrong.GetExpectedNoStream() != nil {
				expected = "no_stream"
			} else if wrong.GetExpectedStreamExists() != nil {
				expected = "stream_exists"
			} else {
				expected = strconv.Itoa(int(wrong.GetExpectedRevision()))
			}

			if wrong.GetCurrentNoStream() != nil {
				current = "no_stream"
			} else {
				current = strconv.Itoa(int(wrong.GetCurrentRevision()))
			}

			return nil, &Error{code: ErrorCodeWrongExpectedVersion, err: fmt.Errorf("wrong expected version: expecting '%s' but got '%s'", expected, current)}
		}
	}

	return &WriteResult{
		CommitPosition:      0,
		PreparePosition:     0,
		NextExpectedVersion: 1,
	}, nil
}

// SetStreamMetadata Sets a stream's metadata.
func (client *Client) SetStreamMetadata(
	context context.Context,
	streamID string,
	opts AppendToStreamOptions,
	metadata StreamMetadata,
) (*WriteResult, error) {
	streamName := fmt.Sprintf("$$%v", streamID)
	props, err := metadata.toMap()

	if err != nil {
		return nil, fmt.Errorf("error when serializing stream metadata: %w", err)
	}

	data, err := json.Marshal(props)

	if err != nil {
		return nil, fmt.Errorf("error when serializing stream metadata: %w", err)
	}

	result, err := client.AppendToStream(context, streamName, opts, EventData{
		ContentType: ContentTypeJson,
		EventType:   "$metadata",
		Data:        data,
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetStreamMetadata Reads a stream's metadata.
func (client *Client) GetStreamMetadata(
	context context.Context,
	streamID string,
	opts ReadStreamOptions,
) (*StreamMetadata, error) {
	streamName := fmt.Sprintf("$$%v", streamID)

	stream, err := client.ReadStream(context, streamName, opts, 1)

	if esdbErr, ok := FromError(err); !ok {
		return nil, esdbErr
	}

	defer stream.Close()
	event, err := stream.Recv()

	if errors.Is(err, io.EOF) {
		return &StreamMetadata{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("unexpected error when reading stream metadata: %w", err)
	}

	var props map[string]interface{}

	err = json.Unmarshal(event.OriginalEvent().Data, &props)

	if err != nil {
		return nil, &Error{code: ErrorCodeParsing, err: fmt.Errorf("error when deserializing stream metadata json: %w", err)}
	}

	meta, err := streamMetadataFromMap(props)

	if err != nil {
		return nil, &Error{code: ErrorCodeParsing, err: fmt.Errorf("error when parsing stream metadata json: %w", err)}
	}

	return meta, nil
}

// DeleteStream Soft deletes a given stream.
//
// Makes use of "Truncate Before". When a stream is deleted, it's "Truncate Before" is set to the stream's current last
// event number. When a soft deleted stream is read, the read will return a stream not found error. After deleting
// the stream, you are able to write to it again, continuing from where it left off.
func (client *Client) DeleteStream(
	parent context.Context,
	streamID string,
	opts DeleteStreamOptions,
) (*DeleteResult, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(parent, client.config, &opts, callOptions)
	defer cancel()
	deleteRequest := toDeleteRequest(streamID, opts.ExpectedRevision)
	deleteResponse, err := streamsClient.Delete(ctx, deleteRequest, callOptions...)
	if err != nil {
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to perform delete, details: %w", err)
	}

	return &DeleteResult{Position: deletePositionFromProto(deleteResponse)}, nil
}

// TombstoneStream Hard deletes a given stream.
//
// A hard delete writes a tombstone event to the stream, permanently deleting it. The stream cannot be recreated or
// written to again. Tombstone events are written with the event's type "$streamDeleted". When a hard deleted stream
// is read, the read will return a stream deleted error.
func (client *Client) TombstoneStream(
	parent context.Context,
	streamID string,
	opts TombstoneStreamOptions,
) (*DeleteResult, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(parent, client.config, &opts, callOptions)
	defer cancel()
	tombstoneRequest := toTombstoneRequest(streamID, opts.ExpectedRevision)
	tombstoneResponse, err := streamsClient.Tombstone(ctx, tombstoneRequest, callOptions...)

	if err != nil {
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to perform delete, details: %w", err)
	}

	return &DeleteResult{Position: tombstonePositionFromProto(tombstoneResponse)}, nil
}

// ReadStream Reads events from a given stream. The reading can be done forward and backward.
func (client *Client) ReadStream(
	context context.Context,
	streamID string,
	opts ReadStreamOptions,
	count uint64,
) (*ReadStream, error) {
	opts.setDefaults()
	readRequest := toReadStreamRequest(streamID, opts.Direction, opts.From, count, opts.ResolveLinkTos)
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())

	return readInternal(context, client, &opts, handle, streamsClient, readRequest)
}

// ReadAll Reads events from the $all stream. The reading can be done forward and backward.
func (client *Client) ReadAll(
	context context.Context,
	opts ReadAllOptions,
	count uint64,
) (*ReadStream, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())
	readRequest := toReadAllRequest(opts.Direction, opts.From, count, opts.ResolveLinkTos)
	return readInternal(context, client, &opts, handle, streamsClient, readRequest)
}

// SubscribeToStream Subscribes to a given stream. This kind of subscription specifies a starting point (by default, the
// beginning of the stream). This subscription will fetch all events until the end of the stream. Then, the subscription
// dispatches subsequently written events. For example, if a starting point of 50 is specified when a stream has 100
// events. The subscriber can expect to see events 51 though 100, and then any events subsequently written until such
// time as the subscriptions.
func (client *Client) SubscribeToStream(
	parent context.Context,
	streamID string,
	opts SubscribeToStreamOptions,
) (*Subscription, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(parent, client.config, &opts, callOptions)

	streamsClient := api.NewStreamsClient(handle.Connection())
	subscriptionRequest, err := toStreamSubscriptionRequest(streamID, opts.From, opts.ResolveLinkTos, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to construct subscription. Reason: %w", err)
	}
	readClient, err := streamsClient.Read(ctx, subscriptionRequest, callOptions...)
	if err != nil {
		defer cancel()
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to construct subscription. Reason: %w", err)
	}
	readResult, err := readClient.Recv()
	if err != nil {
		defer cancel()
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to perform read. Reason: %w", err)
	}
	switch readResult.Content.(type) {
	case *api.ReadResp_Confirmation:
		{
			confirmation := readResult.GetConfirmation()
			return newSubscription(client, cancel, readClient, confirmation.SubscriptionId), nil
		}
	}
	defer cancel()
	return nil, fmt.Errorf("failed to initiate subscription")
}

// SubscribeToAll Subscribes to $all. This kind of subscription specifies a starting point (by default, the beginning
// of a stream). This subscription will fetch all events until the end of the stream. Then, the subscription dispatches
// subsequently written events.
func (client *Client) SubscribeToAll(
	parent context.Context,
	opts SubscribeToAllOptions,
) (*Subscription, error) {
	opts.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}
	streamsClient := api.NewStreamsClient(handle.Connection())
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(parent, client.config, &opts, callOptions)

	var filterOptions *SubscriptionFilterOptions = nil
	if opts.Filter != nil {
		filterOptions = &SubscriptionFilterOptions{
			MaxSearchWindow:    opts.MaxSearchWindow,
			CheckpointInterval: opts.CheckpointInterval,
			SubscriptionFilter: opts.Filter,
		}
	}

	subscriptionRequest, err := toAllSubscriptionRequest(opts.From, opts.ResolveLinkTos, filterOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to construct subscription. Reason: %w", err)
	}
	readClient, err := streamsClient.Read(ctx, subscriptionRequest, callOptions...)
	if err != nil {
		defer cancel()
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to construct subscription. Reason: %w", err)
	}
	readResult, err := readClient.Recv()
	if err != nil {
		defer cancel()
		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, fmt.Errorf("failed to perform read. Reason: %w", err)
	}
	switch readResult.Content.(type) {
	case *api.ReadResp_Confirmation:
		{
			confirmation := readResult.GetConfirmation()
			return newSubscription(client, cancel, readClient, confirmation.SubscriptionId), nil
		}
	}
	defer cancel()
	return nil, fmt.Errorf("failed to initiate subscription")
}

// SubscribeToPersistentSubscription Connects to a persistent subscription group on a stream.
func (client *Client) SubscribeToPersistentSubscription(
	ctx context.Context,
	streamName string,
	groupName string,
	options SubscribeToPersistentSubscriptionOptions,
) (*PersistentSubscription, error) {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}

	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	return persistentSubscriptionClient.ConnectToPersistentSubscription(
		ctx,
		client.config,
		&options,
		handle,
		int32(options.BufferSize),
		streamName,
		groupName,
	)
}

// SubscribeToPersistentSubscriptionToAll Connects to a persistent subscription group to the $all stream.
func (client *Client) SubscribeToPersistentSubscriptionToAll(
	ctx context.Context,
	groupName string,
	options SubscribeToPersistentSubscriptionOptions,
) (*PersistentSubscription, error) {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}

	if !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return nil, unsupportedFeatureError()
	}
	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	return persistentSubscriptionClient.ConnectToPersistentSubscription(
		ctx,
		client.config,
		&options,
		handle,
		int32(options.BufferSize),
		"",
		groupName,
	)
}

// CreatePersistentSubscription Creates a persistent subscription gorup on a stream.
//
// Persistent subscriptions are special kind of subscription where the server remembers the state of the subscription.
// This allows for many modes of operations compared to a regular or catcup subscription where the client
// holds the subscription state.
func (client *Client) CreatePersistentSubscription(
	ctx context.Context,
	streamName string,
	groupName string,
	options PersistentStreamSubscriptionOptions,
) error {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}
	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	if options.Settings == nil {
		setts := SubscriptionSettingsDefault()
		options.Settings = &setts
	}

	return persistentSubscriptionClient.CreateStreamSubscription(ctx, client.config, &options, handle, streamName, groupName, options.StartFrom, *options.Settings)
}

// CreatePersistentSubscriptionToAll Creates a persistent subscription gorup on the $all stream.
//
// Persistent subscriptions are special kind of subscription where the server remembers the state of the subscription.
// This allows for many modes of operations compared to a regular or catcup subscription where the client
// holds the subscription state.
func (client *Client) CreatePersistentSubscriptionToAll(
	ctx context.Context,
	groupName string,
	options PersistentAllSubscriptionOptions,
) error {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}

	if !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return unsupportedFeatureError()
	}
	var filterOptions *SubscriptionFilterOptions = nil
	if options.Filter != nil {
		filterOptions = &SubscriptionFilterOptions{
			MaxSearchWindow:    options.MaxSearchWindow,
			SubscriptionFilter: options.Filter,
		}
	}
	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	if options.Settings == nil {
		setts := SubscriptionSettingsDefault()
		options.Settings = &setts
	}

	return persistentSubscriptionClient.CreateAllSubscription(
		ctx,
		client.config,
		&options,
		handle,
		groupName,
		options.StartFrom,
		*options.Settings,
		filterOptions,
	)
}

// UpdatePersistentSubscription Updates a persistent subscription group on a stream.
func (client *Client) UpdatePersistentSubscription(
	ctx context.Context,
	streamName string,
	groupName string,
	options PersistentStreamSubscriptionOptions,
) error {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}
	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	if options.Settings == nil {
		setts := SubscriptionSettingsDefault()
		options.Settings = &setts
	}

	return persistentSubscriptionClient.UpdateStreamSubscription(ctx, client.config, &options, handle, streamName, groupName, options.StartFrom, *options.Settings)
}

// UpdatePersistentSubscriptionToAll Updates a persistent subscription group on the $all stream.
func (client *Client) UpdatePersistentSubscriptionToAll(
	ctx context.Context,
	groupName string,
	options PersistentAllSubscriptionOptions,
) error {
	options.setDefaults()
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}

	if !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return unsupportedFeatureError()
	}

	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	return persistentSubscriptionClient.UpdateAllSubscription(ctx, client.config, &options, handle, groupName, options.StartFrom, *options.Settings)
}

// DeletePersistentSubscription Deletes a persistent subscription group on a stream.
func (client *Client) DeletePersistentSubscription(
	ctx context.Context,
	streamName string,
	groupName string,
	options DeletePersistentSubscriptionOptions,
) error {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}
	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	return persistentSubscriptionClient.DeleteStreamSubscription(ctx, client.config, &options, handle, streamName, groupName)
}

// DeletePersistentSubscriptionToAll Deletes a persistent subscription group on the $all stream.
func (client *Client) DeletePersistentSubscriptionToAll(
	ctx context.Context,
	groupName string,
	options DeletePersistentSubscriptionOptions,
) error {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}

	if !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return unsupportedFeatureError()
	}

	persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))

	return persistentSubscriptionClient.DeleteAllSubscription(ctx, client.config, &options, handle, groupName)
}

// ReplayParkedMessages Replays a persistent subscription to a stream parked events.
func (client *Client) ReplayParkedMessages(ctx context.Context, streamName string, groupName string, options ReplayParkedMessagesOptions) error {
	return client.replayParkedMessages(ctx, streamName, groupName, options)
}

// ReplayParkedMessagesToAll Replays a persistent subscription to a $all stream parked events.
func (client *Client) ReplayParkedMessagesToAll(ctx context.Context, groupName string, options ReplayParkedMessagesOptions) error {
	return client.replayParkedMessages(ctx, "$all", groupName, options)
}

func (client *Client) replayParkedMessages(ctx context.Context, streamName string, groupName string, options ReplayParkedMessagesOptions) error {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}

	var finalStreamName *string
	if streamName != "$all" {
		finalStreamName = &streamName
	}

	if finalStreamName == nil && !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return unsupportedFeatureError()
	}

	if handle.SupportsFeature(featurePersistentSubscriptionManagement) {
		persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))
		return persistentSubscriptionClient.replayParkedMessages(ctx, client.config, handle, finalStreamName, groupName, options)
	}

	return client.httpReplayParkedMessages(streamName, groupName, options)
}

// ListAllPersistentSubscriptions Lists all persistent subscriptions to date.
func (client *Client) ListAllPersistentSubscriptions(ctx context.Context, options ListPersistentSubscriptionsOptions) ([]PersistentSubscriptionInfo, error) {
	return client.listPersistentSubscriptionsInternal(ctx, nil, options)
}

// ListPersistentSubscriptionsForStream Lists all persistent subscriptions of a specific stream.
func (client *Client) ListPersistentSubscriptionsForStream(ctx context.Context, streamName string, options ListPersistentSubscriptionsOptions) ([]PersistentSubscriptionInfo, error) {
	return client.listPersistentSubscriptionsInternal(ctx, &streamName, options)
}

// ListPersistentSubscriptionsToAll Lists all persistent subscriptions specific to the $all stream.
func (client *Client) ListPersistentSubscriptionsToAll(ctx context.Context, options ListPersistentSubscriptionsOptions) ([]PersistentSubscriptionInfo, error) {
	streamName := "$all"
	return client.listPersistentSubscriptionsInternal(ctx, &streamName, options)
}

func (client *Client) listPersistentSubscriptionsInternal(ctx context.Context, streamName *string, options ListPersistentSubscriptionsOptions) ([]PersistentSubscriptionInfo, error) {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}

	if streamName != nil && *streamName == "$all" && !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return nil, unsupportedFeatureError()
	}

	if handle.SupportsFeature(featurePersistentSubscriptionManagement) {
		persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))
		return persistentSubscriptionClient.listPersistentSubscriptions(ctx, client.config, handle, streamName, options)
	}

	if streamName != nil {
		return client.httpListPersistentSubscriptionsForStream(*streamName, options)
	}

	return client.httpListAllPersistentSubscriptions(options)
}

// GetPersistentSubscriptionInfo Gets a specific persistent subscription info to a stream.
func (client *Client) GetPersistentSubscriptionInfo(ctx context.Context, streamName string, groupName string, options GetPersistentSubscriptionOptions) (*PersistentSubscriptionInfo, error) {
	return client.getPersistentSubscriptionInfoInternal(ctx, &streamName, groupName, options)
}

// GetPersistentSubscriptionInfoToAll Gets a specific persistent subscription info to the $all stream.
func (client *Client) GetPersistentSubscriptionInfoToAll(ctx context.Context, groupName string, options GetPersistentSubscriptionOptions) (*PersistentSubscriptionInfo, error) {
	return client.getPersistentSubscriptionInfoInternal(ctx, nil, groupName, options)
}

func (client *Client) getPersistentSubscriptionInfoInternal(ctx context.Context, streamName *string, groupName string, options GetPersistentSubscriptionOptions) (*PersistentSubscriptionInfo, error) {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return nil, err
	}

	if streamName != nil && *streamName == "$all" && !handle.SupportsFeature(featurePersistentSubscriptionToAll) {
		return nil, unsupportedFeatureError()

	}

	if handle.SupportsFeature(featurePersistentSubscriptionManagement) {
		persistentSubscriptionClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))
		return persistentSubscriptionClient.getPersistentSubscriptionInfo(ctx, client.config, handle, streamName, groupName, options)
	}

	if streamName == nil {
		streamName = new(string)
		*streamName = "$all"
	}

	return client.httpGetPersistentSubscriptionInfo(*streamName, groupName, options)
}

// RestartPersistentSubscriptionSubsystem Restarts the server persistent subscription subsystem.
func (client *Client) RestartPersistentSubscriptionSubsystem(ctx context.Context, options RestartPersistentSubscriptionSubsystemOptions) error {
	handle, err := client.grpcClient.getConnectionHandle()
	if err != nil {
		return err
	}

	if handle.SupportsFeature(featurePersistentSubscriptionManagement) {
		persistentClient := newPersistentClient(client.grpcClient, persistentProto.NewPersistentSubscriptionsClient(handle.Connection()))
		return persistentClient.restartSubsystem(ctx, client.config, handle, options)
	}

	return client.httpRestartSubsystem(options)
}

func readInternal(
	parent context.Context,
	client *Client,
	options options,
	handle *connectionHandle,
	streamsClient api.StreamsClient,
	readRequest *api.ReadReq,
) (*ReadStream, error) {
	var headers, trailers metadata.MD
	callOptions := []grpc.CallOption{grpc.Header(&headers), grpc.Trailer(&trailers)}
	callOptions, ctx, cancel := configureGrpcCall(parent, client.config, options, callOptions)
	result, err := streamsClient.Read(ctx, readRequest, callOptions...)
	if err != nil {
		defer cancel()

		err = client.grpcClient.handleError(handle, headers, trailers, err)
		return nil, &Error{
			code: ErrorCodeUnknown,
			err:  fmt.Errorf("failed to construct read stream. Reason: %w", err),
		}
	}

	params := readStreamParams{
		client:   client.grpcClient,
		handle:   handle,
		cancel:   cancel,
		inner:    result,
		headers:  &headers,
		trailers: &trailers,
	}

	return newReadStream(params), nil
}
