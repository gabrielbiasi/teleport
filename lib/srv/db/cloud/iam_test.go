/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/srv/db/common"
	"github.com/gravitational/trace"
	"github.com/mailgun/timetools"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/aws/aws-sdk-go/service/redshift"

	"github.com/stretchr/testify/require"
)

// TestAWSIAM tests RDS, Aurora and Redshift IAM auto-configuration.
func TestAWSIAM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Setup AWS database objects.
	rdsInstance := &rds.DBInstance{
		DBInstanceArn:        aws.String("arn:aws:rds:us-west-1:1234567890:db:postgres-rds"),
		DBInstanceIdentifier: aws.String("postgres-rds"),
		DbiResourceId:        aws.String("db-xyz"),
	}

	auroraCluster := &rds.DBCluster{
		DBClusterArn:        aws.String("arn:aws:rds:us-east-1:1234567890:cluster:postgres-aurora"),
		DBClusterIdentifier: aws.String("postgres-aurora"),
		DbClusterResourceId: aws.String("cluster-xyz"),
	}

	redshiftCluster := &redshift.Cluster{
		ClusterNamespaceArn: aws.String("arn:aws:redshift:us-east-2:1234567890:namespace:namespace-xyz"),
		ClusterIdentifier:   aws.String("redshift-cluster-1"),
	}

	// Configure mocks.
	stsClient := &STSMock{
		ARN: "arn:aws:iam::1234567890:role/test-role",
	}

	rdsClient := &RDSMock{
		DBInstances: []*rds.DBInstance{rdsInstance},
		DBClusters:  []*rds.DBCluster{auroraCluster},
	}

	redshiftClient := &RedshiftMock{
		Clusters: []*redshift.Cluster{redshiftCluster},
	}

	iamClient := &IAMMock{}

	limiterClock := timetools.SleepProvider(time.Now())
	limiter, err := limiter.NewRateLimiter(limiter.Config{
		Rates: []limiter.Rate{{
			Period:  time.Hour,
			Average: 1,
			Burst:   1,
		}},
		Clock: limiterClock,
	})
	require.NoError(t, err)

	// Setup database resources.
	rdsDatabase, err := types.NewDatabaseV3(types.Metadata{
		Name: "postgres-rds",
	}, types.DatabaseSpecV3{
		Protocol: defaults.ProtocolPostgres,
		URI:      "localhost",
		AWS:      types.AWS{Region: "localhost", AccountID: "1234567890", RDS: types.RDS{InstanceID: "postgres-rds", ResourceID: "postgres-rds-resource-id"}},
	})
	require.NoError(t, err)

	auroraDatabase, err := types.NewDatabaseV3(types.Metadata{
		Name: "postgres-aurora",
	}, types.DatabaseSpecV3{
		Protocol: defaults.ProtocolPostgres,
		URI:      "localhost",
		AWS:      types.AWS{Region: "localhost", AccountID: "1234567890", RDS: types.RDS{ClusterID: "postgres-aurora", ResourceID: "postgres-aurora-resource-id"}},
	})
	require.NoError(t, err)

	redshiftDatabase, err := types.NewDatabaseV3(types.Metadata{
		Name: "redshift",
	}, types.DatabaseSpecV3{
		Protocol: defaults.ProtocolPostgres,
		URI:      "localhost",
		AWS:      types.AWS{Region: "localhost", AccountID: "1234567890", Redshift: types.Redshift{ClusterID: "redshift-cluster-1"}},
	})
	require.NoError(t, err)

	databaseMissingMetadata, err := types.NewDatabaseV3(types.Metadata{
		Name: "redshift",
	}, types.DatabaseSpecV3{
		Protocol: defaults.ProtocolPostgres,
		URI:      "localhost",
		AWS:      types.AWS{Redshift: types.Redshift{ClusterID: "missing metadata"}},
	})
	require.NoError(t, err)

	// Make configurator.
	configurator, err := NewIAM(ctx, IAMConfig{
		Semaphores: &SemaphoresMock{},
		Clients: &common.TestCloudClients{
			RDS:      rdsClient,
			Redshift: redshiftClient,
			STS:      stsClient,
			IAM:      iamClient,
		},
		HostID:           "host-id",
		SetupRateLimiter: limiter,
	})
	require.NoError(t, err)
	go configurator.Start()

	t.Run("RDS", func(t *testing.T) {
		// Configure RDS database and make sure IAM was enabled and policy was attached.
		err = configurator.Setup(ctx, rdsDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		require.True(t, aws.BoolValue(rdsInstance.IAMDatabaseAuthenticationEnabled))
		policy := iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.Contains(t, policy, rdsDatabase.GetAWS().RDS.ResourceID)

		// Deconfigure RDS database, policy should get detached.
		err = configurator.Teardown(ctx, rdsDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		policy = iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.NotContains(t, policy, rdsDatabase.GetAWS().RDS.ResourceID)
	})

	t.Run("Aurora", func(t *testing.T) {
		// Configure Aurora database and make sure IAM was enabled and policy was attached.
		err = configurator.Setup(ctx, auroraDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		require.True(t, aws.BoolValue(auroraCluster.IAMDatabaseAuthenticationEnabled))
		policy := iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.Contains(t, policy, auroraDatabase.GetAWS().RDS.ResourceID)

		// Deconfigure Aurora database, policy should get detached.
		err = configurator.Teardown(ctx, auroraDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		policy = iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.NotContains(t, policy, auroraDatabase.GetAWS().RDS.ResourceID)
	})

	t.Run("Redshift", func(t *testing.T) {
		// Configure Redshift database and make sure policy was attached.
		err = configurator.Setup(ctx, redshiftDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		policy := iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.Contains(t, policy, redshiftDatabase.GetAWS().Redshift.ClusterID)

		// Deconfigure Redshift database, policy should get detached.
		err = configurator.Teardown(ctx, redshiftDatabase)
		require.NoError(t, err)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		policy = iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.NotContains(t, policy, redshiftDatabase.GetAWS().Redshift.ClusterID)
	})

	t.Run("rate limiting setup", func(t *testing.T) {
		// Setup immediately for the same database should be rate limited.
		err = configurator.Setup(ctx, redshiftDatabase)
		require.True(t, trace.IsLimitExceeded(err), "expect err is trace.LimitExceeded")

		// Rate limit is lifted after advancing time.
		timetools.AdvanceTimeBy(limiterClock, 2*time.Hour)
		err = configurator.Teardown(ctx, redshiftDatabase)
		require.NoError(t, err)
	})

	t.Run("missing metadata", func(t *testing.T) {
		// Database without enough metadata to generate IAM actions should not
		// be added to the policy.
		err = configurator.Setup(ctx, databaseMissingMetadata)
		require.Eventuallyf(t, configurator.isIdle, 10*time.Second, 50*time.Millisecond, "database is not processed")
		policy := iamClient.attachedRolePolicies["test-role"][databaseAccessInlinePolicyName]
		require.NotContains(t, policy, databaseMissingMetadata.GetAWS().Redshift.ClusterID)
	})
}

// TestAWSIAMNoPermissions tests that lack of AWS permissions does not produce
// errors during IAM auto-configuration.
func TestAWSIAMNoPermissions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Create unauthorized mocks for AWS services.
	stsClient := &STSMock{
		ARN: "arn:aws:iam::1234567890:role/test-role",
	}
	rdsClient := &RDSMockUnauth{}
	redshiftClient := &RedshiftMockUnauth{}
	iamClient := &IAMMockUnauth{}

	// Make configurator.
	configurator, err := NewIAM(ctx, IAMConfig{
		Semaphores: &SemaphoresMock{},
		Clients: &common.TestCloudClients{
			STS:      stsClient,
			RDS:      rdsClient,
			Redshift: redshiftClient,
			IAM:      iamClient,
		},
		HostID: "host-id",
	})
	require.NoError(t, err)

	tests := []struct {
		name string
		meta types.AWS
	}{
		{
			name: "RDS database",
			meta: types.AWS{Region: "localhost", AccountID: "1234567890", RDS: types.RDS{InstanceID: "postgres-rds", ResourceID: "postgres-rds-resource-id"}},
		},
		{
			name: "Aurora cluster",
			meta: types.AWS{Region: "localhost", AccountID: "1234567890", RDS: types.RDS{ClusterID: "postgres-aurora", ResourceID: "postgres-aurora-resource-id"}},
		},
		{
			name: "Redshift cluster",
			meta: types.AWS{Region: "localhost", AccountID: "1234567890", Redshift: types.Redshift{ClusterID: "redshift-cluster-1"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, err := types.NewDatabaseV3(types.Metadata{
				Name: "test",
			}, types.DatabaseSpecV3{
				Protocol: defaults.ProtocolPostgres,
				URI:      "localhost",
				AWS:      test.meta,
			})
			require.NoError(t, err)

			// Make sure there're no errors trying to setup/destroy IAM.
			err = configurator.processTask(ctx, iamTask{
				isSetup:  true,
				database: database,
			})
			require.NoError(t, err)

			err = configurator.processTask(ctx, iamTask{
				isSetup:  false,
				database: database,
			})
			require.NoError(t, err)
		})
	}
}

// DELETE IN 11.0.
func TestAWSIAMMigration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Configure mocks.
	stsClient := &STSMock{
		ARN: "arn:aws:iam::1234567890:role/test-role",
	}

	iamClient := &IAMMock{
		attachedRolePolicies: map[string]map[string]string{
			"test-role": map[string]string{
				"teleport-host-id": "old policy",
			},
		},
	}

	// Make configurator.
	configurator, err := NewIAM(ctx, IAMConfig{
		Semaphores: &SemaphoresMock{},
		Clients: &common.TestCloudClients{
			STS: stsClient,
			IAM: iamClient,
		},
		HostID: "host-id",
	})
	require.NoError(t, err)
	configurator.migrateInlinePolicy(ctx)

	_, err = iamClient.GetRolePolicyWithContext(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String("test-role"),
		PolicyName: aws.String("teleport-host-id"),
	})
	require.True(t, trace.IsNotFound(common.ConvertError(err)), "expect err is trace.NotFound")
}
