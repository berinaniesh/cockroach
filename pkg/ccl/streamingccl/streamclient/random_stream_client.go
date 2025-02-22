// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package streamclient

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/valueside"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/streaming"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	// RandomStreamSchemaPlaceholder is the schema of the KVs emitted by the
	// random stream client.
	RandomStreamSchemaPlaceholder = "CREATE TABLE %s (k INT PRIMARY KEY, v INT)"

	// RandomGenScheme is the URI scheme used to create a test load.
	RandomGenScheme = "randomgen"
	// ValueRangeKey controls the range of the randomly generated values produced
	// by this workload. The workload will generate between 0 and this value.
	ValueRangeKey = "VALUE_RANGE"
	// EventFrequency is the frequency in nanoseconds that the stream will emit
	// randomly generated KV events.
	EventFrequency = "EVENT_FREQUENCY"
	// KVsPerCheckpoint controls approximately how many KV events should be emitted
	// between checkpoint events.
	KVsPerCheckpoint = "KVS_PER_CHECKPOINT"
	// NumPartitions controls the number of partitions the client will stream data
	// back on. Each partition will encompass a single table span.
	NumPartitions = "NUM_PARTITIONS"
	// DupProbability controls the probability with which we emit duplicate KV
	// events.
	DupProbability = "DUP_PROBABILITY"
	// TenantID specifies the ID of the tenant we are ingesting data into. This
	// allows the client to prefix the generated KVs with the appropriate tenant
	// prefix.
	TenantID = "TENANT_ID"
	// IngestionDatabaseID is the ID used in the generated table descriptor.
	IngestionDatabaseID = 50 /* defaultDB */
	// IngestionTablePrefix is the prefix of the table name used in the generated
	// table descriptor.
	IngestionTablePrefix = "foo"
)

// TODO(dt): just make interceptors a singleton, not the whole client.
var randomStreamClientSingleton = func() *randomStreamClient {
	c := randomStreamClient{}
	c.mu.tableID = 52
	return &c
}()

// GetRandomStreamClientSingletonForTesting returns the singleton instance of
// the client. This is to be used in testing, when interceptors can be
// registered on the client to observe events.
func GetRandomStreamClientSingletonForTesting() Client {
	return randomStreamClientSingleton
}

// InterceptFn is a function that will intercept events emitted by
// an InterceptableStreamClient
type InterceptFn func(event streamingccl.Event, spec SubscriptionToken)

// InterceptableStreamClient wraps a Client, and provides a method to register
// interceptor methods that are run on every streamed Event.
type InterceptableStreamClient interface {
	Client

	// RegisterInterception is how you can register your interceptor to be called
	// from an InterceptableStreamClient.
	RegisterInterception(fn InterceptFn)
}

// randomStreamConfig specifies the variables that controls the rate and type of
// events that the generated stream emits.
type randomStreamConfig struct {
	valueRange       int
	eventFrequency   time.Duration
	kvsPerCheckpoint int
	numPartitions    int
	dupProbability   float64
	tenantID         roachpb.TenantID
}

func parseRandomStreamConfig(streamURL *url.URL) (randomStreamConfig, error) {
	c := randomStreamConfig{
		valueRange:       100,
		eventFrequency:   10 * time.Microsecond,
		kvsPerCheckpoint: 100,
		numPartitions:    1,
		dupProbability:   0.5,
		tenantID:         roachpb.SystemTenantID,
	}

	var err error
	if valueRangeStr := streamURL.Query().Get(ValueRangeKey); valueRangeStr != "" {
		c.valueRange, err = strconv.Atoi(valueRangeStr)
		if err != nil {
			return c, err
		}
	}

	if kvFreqStr := streamURL.Query().Get(EventFrequency); kvFreqStr != "" {
		kvFreq, err := strconv.Atoi(kvFreqStr)
		c.eventFrequency = time.Duration(kvFreq)
		if err != nil {
			return c, err
		}
	}

	if kvsPerCheckpointStr := streamURL.Query().Get(KVsPerCheckpoint); kvsPerCheckpointStr != "" {
		c.kvsPerCheckpoint, err = strconv.Atoi(kvsPerCheckpointStr)
		if err != nil {
			return c, err
		}
	}

	if numPartitionsStr := streamURL.Query().Get(NumPartitions); numPartitionsStr != "" {
		c.numPartitions, err = strconv.Atoi(numPartitionsStr)
		if err != nil {
			return c, err
		}
	}

	if dupProbStr := streamURL.Query().Get(DupProbability); dupProbStr != "" {
		c.dupProbability, err = strconv.ParseFloat(dupProbStr, 32)
		if err != nil {
			return c, err
		}
	}

	if tenantIDStr := streamURL.Query().Get(TenantID); tenantIDStr != "" {
		id, err := strconv.Atoi(tenantIDStr)
		if err != nil {
			return c, err
		}
		c.tenantID = roachpb.MakeTenantID(uint64(id))
	}
	return c, nil
}

func (c randomStreamConfig) URL(table int) string {
	u := &url.URL{
		Scheme: RandomGenScheme,
		Host:   strconv.Itoa(table),
	}
	q := u.Query()
	q.Add(ValueRangeKey, strconv.Itoa(c.valueRange))
	q.Add(EventFrequency, strconv.Itoa(int(c.eventFrequency)))
	q.Add(KVsPerCheckpoint, strconv.Itoa(c.kvsPerCheckpoint))
	q.Add(NumPartitions, strconv.Itoa(c.numPartitions))
	q.Add(DupProbability, fmt.Sprintf("%f", c.dupProbability))
	q.Add(TenantID, strconv.Itoa(int(c.tenantID.ToUint64())))
	u.RawQuery = q.Encode()
	return u.String()
}

// randomStreamClient is a temporary stream client implementation that generates
// random events.
//
// The client can be configured to return more than one partition via the stream
// URL. Each partition covers a single table span.
type randomStreamClient struct {
	config randomStreamConfig

	// mu is used to provide a threadsafe interface to interceptors.
	mu struct {
		syncutil.Mutex

		// interceptors can be registered to peek at every event generated by this
		// client and which partition spec it was sent to.
		interceptors []func(streamingccl.Event, SubscriptionToken)
		tableID      int
	}
}

var _ Client = &randomStreamClient{}
var _ InterceptableStreamClient = &randomStreamClient{}

// newRandomStreamClient returns a stream client that generates a random set of
// events on a table with an integer key and integer value for the table with
// the given ID.
func newRandomStreamClient(streamURL *url.URL) (Client, error) {
	c := randomStreamClientSingleton

	streamConfig, err := parseRandomStreamConfig(streamURL)
	if err != nil {
		return nil, err
	}
	c.config = streamConfig
	return c, nil
}

func (m *randomStreamClient) getNextTableID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	ret := m.mu.tableID
	m.mu.tableID++
	return ret
}

// Plan implements the Client interface.
func (m *randomStreamClient) Plan(ctx context.Context, id streaming.StreamID) (Topology, error) {
	topology := make(Topology, 0, m.config.numPartitions)
	log.Infof(ctx, "planning random stream for tenant %d", m.config.tenantID)

	// Allocate table IDs and return one per partition address in the topology.
	for i := 0; i < m.config.numPartitions; i++ {
		partitionURI := m.config.URL(m.getNextTableID())
		log.Infof(ctx, "planning random stream partition %d for tenant %d: %q", i, m.config.tenantID, partitionURI)

		topology = append(topology,
			PartitionInfo{
				ID:                strconv.Itoa(i),
				SrcAddr:           streamingccl.PartitionAddress(partitionURI),
				SubscriptionToken: []byte(partitionURI),
			})
	}

	return topology, nil
}

// Create implements the Client interface.
func (m *randomStreamClient) Create(
	ctx context.Context, target roachpb.TenantID,
) (streaming.StreamID, error) {
	log.Infof(ctx, "creating random stream for tenant %d", target.ToUint64())
	m.config.tenantID = target
	return streaming.StreamID(target.ToUint64()), nil
}

// Heartbeat implements the Client interface.
func (m *randomStreamClient) Heartbeat(
	ctx context.Context, ID streaming.StreamID, _ hlc.Timestamp,
) error {
	return nil
}

// getDescriptorAndNamespaceKVForTableID returns the namespace and descriptor
// KVs for the table with tableID.
func (m *randomStreamClient) getDescriptorAndNamespaceKVForTableID(
	config randomStreamConfig, tableID descpb.ID,
) (*tabledesc.Mutable, []roachpb.KeyValue, error) {
	tableName := fmt.Sprintf("%s%d", IngestionTablePrefix, tableID)
	testTable, err := sql.CreateTestTableDescriptor(
		context.Background(),
		IngestionDatabaseID,
		tableID,
		fmt.Sprintf(RandomStreamSchemaPlaceholder, tableName),
		descpb.NewBasePrivilegeDescriptor(security.RootUserName()),
	)
	if err != nil {
		return nil, nil, err
	}

	// Generate namespace entry.
	codec := keys.MakeSQLCodec(config.tenantID)
	key := catalogkeys.MakePublicObjectNameKey(codec, 50, testTable.Name)
	k := rekey(config.tenantID, key)
	var value roachpb.Value
	value.SetInt(int64(testTable.GetID()))
	value.InitChecksum(k)
	namespaceKV := roachpb.KeyValue{
		Key:   k,
		Value: value,
	}

	// Generate descriptor entry.
	descKey := catalogkeys.MakeDescMetadataKey(codec, testTable.GetID())
	descKey = rekey(config.tenantID, descKey)
	descDesc := testTable.DescriptorProto()
	var descValue roachpb.Value
	if err := descValue.SetProto(descDesc); err != nil {
		panic(err)
	}
	descValue.InitChecksum(descKey)
	descKV := roachpb.KeyValue{
		Key:   descKey,
		Value: descValue,
	}

	return testTable, []roachpb.KeyValue{namespaceKV, descKV}, nil
}

// Close implements the Client interface.
func (m *randomStreamClient) Close() error {
	return nil
}

// Subscribe implements the Client interface.
func (m *randomStreamClient) Subscribe(
	ctx context.Context, stream streaming.StreamID, spec SubscriptionToken, checkpoint hlc.Timestamp,
) (Subscription, error) {
	partitionURL, err := url.Parse(string(spec))
	if err != nil {
		return nil, err
	}
	config, err := parseRandomStreamConfig(partitionURL)
	if err != nil {
		return nil, err
	}

	eventCh := make(chan streamingccl.Event)
	now := timeutil.Now()
	startWalltime := timeutil.Unix(0 /* sec */, checkpoint.WallTime)
	if startWalltime.After(now) {
		panic("cannot start random stream client event stream in the future")
	}

	var partitionTableID int
	partitionTableID, err = strconv.Atoi(partitionURL.Host)
	if err != nil {
		return nil, err
	}
	log.Infof(ctx, "producing kvs for metadata for table %d for tenant %d based on %q", partitionTableID, config.tenantID, spec)
	tableDesc, systemKVs, err := m.getDescriptorAndNamespaceKVForTableID(config, descpb.ID(partitionTableID))
	if err != nil {
		return nil, err
	}
	receiveFn := func(ctx context.Context) error {
		defer close(eventCh)

		// rand is not thread safe, so create a random source for each partition.
		r := rand.New(rand.NewSource(timeutil.Now().UnixNano()))
		kvInterval := config.eventFrequency

		numKVEventsSinceLastResolved := 0

		rng, _ := randutil.NewPseudoRand()
		var dupKVEvent streamingccl.Event

		for {
			var event streamingccl.Event
			if numKVEventsSinceLastResolved == config.kvsPerCheckpoint {
				// Emit a CheckpointEvent.
				resolvedTime := timeutil.Now()
				hlcResolvedTime := hlc.Timestamp{WallTime: resolvedTime.UnixNano()}
				event = streamingccl.MakeCheckpointEvent(hlcResolvedTime)
				dupKVEvent = nil

				numKVEventsSinceLastResolved = 0
			} else {
				// If there are system KVs to emit, prioritize those.
				if len(systemKVs) > 0 {
					systemKV := systemKVs[0]
					systemKV.Value.Timestamp = hlc.Timestamp{WallTime: timeutil.Now().UnixNano()}
					event = streamingccl.MakeKVEvent(systemKV)
					systemKVs = systemKVs[1:]
				} else {
					numKVEventsSinceLastResolved++
					// Generate a duplicate KVEvent.
					if rng.Float64() < config.dupProbability && dupKVEvent != nil {
						dupKV := dupKVEvent.GetKV()
						event = streamingccl.MakeKVEvent(*dupKV)
					} else {
						event = streamingccl.MakeKVEvent(makeRandomKey(r, config, tableDesc))
						dupKVEvent = event
					}
				}
			}

			select {
			case eventCh <- event:
			case <-ctx.Done():
				return ctx.Err()
			}

			func() {
				m.mu.Lock()
				defer m.mu.Unlock()

				if len(m.mu.interceptors) > 0 {
					for _, interceptor := range m.mu.interceptors {
						if interceptor != nil {
							interceptor(event, spec)
						}
					}
				}
			}()

			time.Sleep(kvInterval)
		}
	}

	return &randomStreamSubscription{
		receiveFn: receiveFn,
		eventCh:   eventCh,
	}, nil
}

type randomStreamSubscription struct {
	receiveFn func(ctx context.Context) error
	eventCh   chan streamingccl.Event
}

// Subscribe implements the Subscription interface.
func (r *randomStreamSubscription) Subscribe(ctx context.Context) error {
	return r.receiveFn(ctx)
}

// Events implements the Subscription interface.
func (r *randomStreamSubscription) Events() <-chan streamingccl.Event {
	return r.eventCh
}

// Err implements the Subscription interface.
func (r *randomStreamSubscription) Err() error {
	return nil
}

func rekey(tenantID roachpb.TenantID, k roachpb.Key) roachpb.Key {
	// Strip old prefix.
	tenantPrefix := keys.MakeTenantPrefix(tenantID)
	noTenantPrefix, _, err := keys.DecodeTenantPrefix(k)
	if err != nil {
		panic(err)
	}

	// Prepend tenant prefix.
	rekeyedKey := append(tenantPrefix, noTenantPrefix...)
	return rekeyedKey
}

func makeRandomKey(
	r *rand.Rand, config randomStreamConfig, tableDesc *tabledesc.Mutable,
) roachpb.KeyValue {
	// Create a key holding a random integer.
	keyDatum := tree.NewDInt(tree.DInt(r.Intn(config.valueRange)))

	index := tableDesc.GetPrimaryIndex()
	// Create the ColumnID to index in datums slice map needed by
	// MakeIndexKeyPrefix.
	var colIDToRowIndex catalog.TableColMap
	colIDToRowIndex.Set(index.GetKeyColumnID(0), 0)

	keyPrefix := rowenc.MakeIndexKeyPrefix(keys.SystemSQLCodec, tableDesc.GetID(), index.GetID())
	k, _, err := rowenc.EncodeIndexKey(tableDesc, index, colIDToRowIndex, tree.Datums{keyDatum}, keyPrefix)
	if err != nil {
		panic(err)
	}
	k = keys.MakeFamilyKey(k, uint32(tableDesc.Families[0].ID))

	k = rekey(config.tenantID, k)

	// Create a value holding a random integer.
	valueDatum := tree.NewDInt(tree.DInt(r.Intn(config.valueRange)))
	valueBuf, err := valueside.Encode(
		[]byte(nil), valueside.MakeColumnIDDelta(0, tableDesc.Columns[1].ID), valueDatum, []byte(nil))
	if err != nil {
		panic(err)
	}
	var v roachpb.Value
	v.SetTuple(valueBuf)
	v.ClearChecksum()
	v.InitChecksum(k)

	v.Timestamp = hlc.Timestamp{WallTime: timeutil.Now().UnixNano()}

	return roachpb.KeyValue{
		Key:   k,
		Value: v,
	}
}

// RegisterInterception implements the InterceptableStreamClient interface.
func (m *randomStreamClient) RegisterInterception(fn InterceptFn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mu.interceptors = append(m.mu.interceptors, fn)
}
