// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package migrations

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/migration"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descbuilder"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

type descIDAndVersion struct {
	id      descpb.ID
	version descpb.DescriptorVersion
}

// runRemoveInvalidDatabasePrivileges calls RunPostDeserializationChanges on
// every database descriptor.
// This migration is done to convert invalid privileges on the
// database to default privileges.
func runRemoveInvalidDatabasePrivileges(
	ctx context.Context, _ clusterversion.ClusterVersion, d migration.TenantDeps, _ *jobs.Job,
) error {
	fixDescriptorFunc := func(ids []descpb.ID, descs []descpb.Descriptor, timestamps []hlc.Timestamp) error {
		var descIDAndVersions []descIDAndVersion
		for i, id := range ids {
			b := descbuilder.NewBuilderWithMVCCTimestamp(&descs[i], timestamps[i])
			if b == nil {
				return errors.Newf("unable to find descriptor for id %d", id)
			}

			b.RunPostDeserializationChanges()
			mutableDesc := b.BuildExistingMutable()

			if mutableDesc.HasPostDeserializationChanges() {
				// Only need to fix the descriptor if there was a change.
				descIDAndVersions = append(descIDAndVersions, descIDAndVersion{
					id:      mutableDesc.GetID(),
					version: mutableDesc.GetVersion(),
				})
			}
		}
		if err := fixDescriptors(ctx, d, descIDAndVersions); err != nil {
			return err
		}
		return nil
	}

	query := `SELECT id, descriptor, crdb_internal_mvcc_timestamp FROM system.descriptor ORDER BY ID ASC`
	rows, err := d.InternalExecutor.QueryIterator(
		ctx, "fix-privileges", nil /* txn */, query,
	)
	if err != nil {
		return err
	}

	return descriptorUpgradeMigration(ctx, rows, fixDescriptorFunc, 1<<19 /* 512 KiB batch size */)
}

// fixDescriptors grabs a descriptor using its ID and fixes the descriptor
// by running RunPostDeserializationChanges.
// The descriptor will only be fixed if the version written to disk is the same
// as the version provided in the array.
func fixDescriptors(
	ctx context.Context, d migration.TenantDeps, descriptorIDAndVersions []descIDAndVersion,
) error {
	return d.CollectionFactory.Txn(ctx, d.InternalExecutor, d.DB, func(
		ctx context.Context, txn *kv.Txn, descriptors *descs.Collection,
	) error {
		batch := txn.NewBatch()
		var fixedIDs []descpb.ID
		for _, idAndVersion := range descriptorIDAndVersions {
			// GetMutableDescriptorByID calls RunPostDeserializationChanges which
			// fixes the descriptor.
			desc, err := descriptors.GetMutableDescriptorByID(ctx, txn, idAndVersion.id)
			if err != nil {
				return err
			}
			if desc.GetVersion() > idAndVersion.version {
				// Already rewritten.
				return nil
			}
			err = descriptors.WriteDescToBatch(ctx, false /* kvTrace */, desc, batch)
			if err != nil {
				return err
			}
			fixedIDs = append(fixedIDs, desc.GetID())
		}
		log.Infof(ctx, "upgrading descriptor with ids %v", fixedIDs)
		return txn.Run(ctx, batch)
	})
}

// fixDescriptorsFunction is used in descriptorUpgradeMigration to fix a set
// of descriptors specified by the id.
type fixDescriptorsFunction func(ids []descpb.ID, descs []descpb.Descriptor, timestamps []hlc.Timestamp) error

// descriptorUpgradeMigration is an abstraction for a descriptor upgrade migration.
// The rows provided should be the result of a select ID, descriptor, crdb_internal_mvcc_timestamp
// from system.descriptor table.
// The datums returned from the query are parsed to grab the descpb.Descriptor
// and fixDescriptorsFunction is called on the desc.
// If minBatchSizeInBytes is specified, fixDescriptors will only be called once the
// size of the descriptors in the id array surpasses minBatchSizeInBytes.
func descriptorUpgradeMigration(
	ctx context.Context,
	rows sqlutil.InternalRows,
	fixDescFunc fixDescriptorsFunction,
	minBatchSizeInBytes int,
) error {
	defer func() { _ = rows.Close() }()
	ok, err := rows.Next(ctx)
	if err != nil {
		return err
	}
	currSize := 0 // in bytes.
	var ids []descpb.ID
	var descs []descpb.Descriptor
	var timestamps []hlc.Timestamp
	for ; ok; ok, err = rows.Next(ctx) {
		if err != nil {
			return err
		}
		datums := rows.Cur()
		id, desc, ts, err := unmarshalDescFromDescriptorRow(datums)
		if err != nil {
			return err
		}
		// If the descriptor is not a database descriptor, we can skip it.
		_, databaseDesc, _, _ := descpb.FromDescriptorWithMVCCTimestamp(&desc, ts)
		if databaseDesc == nil {
			continue
		}
		ids = append(ids, id)
		descs = append(descs, desc)
		timestamps = append(timestamps, ts)
		currSize += desc.Size()
		if currSize > minBatchSizeInBytes || minBatchSizeInBytes == 0 {
			err = fixDescFunc(ids, descs, timestamps)
			if err != nil {
				return err
			}
			// Reset size and id array after the batch is fixed.
			currSize = 0
			ids = nil
			descs = nil
			timestamps = nil
		}
	}
	// Fix remaining descriptors.
	return fixDescFunc(ids, descs, timestamps)
}
