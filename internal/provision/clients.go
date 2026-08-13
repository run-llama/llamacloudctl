package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	redis "google.golang.org/api/redis/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"k8s.io/client-go/kubernetes"
)

// This file builds the concrete cloud + Kubernetes clients the provisioners use,
// so the wizard can CONSTRUCT a provisioner (pure — EnsureParams only) and the
// real clients are resolved lazily at Ensure time. Auth is the operator's default
// credential mechanism on every cloud — awsconfig.LoadDefaultConfig on AWS,
// Application Default Credentials on GCP — mirroring internal/install
// s3provision.go. There is no static-cred branch on either (an earlier design
// decision).

// ClientConfig carries the non-cloud inputs a provisioner needs to build its
// Kubernetes clientset for the credential Secret write. Both fields are optional
// (empty falls back to the default kube loading rules).
type ClientConfig struct {
	Kubeconfig string
	Context    string
}

// awsConfig resolves an AWS config for the region via the default credential
// chain. No network call is made here (credential resolution is lazy). It returns
// one client per managed-service surface (rds, elasticache); each caller keeps the
// client it needs and ignores the other with a blank.
func awsConfig(ctx context.Context, region string) (rdsAPI, elasticacheAPI, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, nil, fmt.Errorf("load AWS config: %w", err)
	}
	return rds.NewFromConfig(cfg), elasticache.NewFromConfig(cfg), nil
}

// gcpClients resolves the Cloud SQL Admin and Memorystore clients via Application
// Default Credentials — the GCP analog of awsConfig's default-credential-chain
// posture, with no static-credential branch. It returns one client per managed
// service surface; each caller keeps the one it needs.
//
// UNLIKE awsConfig, this DOES resolve credentials eagerly: sqladmin.NewService and
// redis.NewService both call htransport.NewClient at construction, which resolves
// ADC there and then. So the "no network call is made here" note on awsConfig is
// an AWS-specific claim and must NOT be copied onto this function.
//
// What keeps the provisioner constructors pure is therefore PLACEMENT, not SDK
// behaviour: NewCloudSQLProvisioner / NewMemorystoreProvisioner build no client,
// and this is called only from the lazy initClients inside Ensure. Do not hoist it
// into a constructor.
func gcpClients(ctx context.Context) (cloudSQLAPI, memorystoreAPI, error) {
	sqlSvc, err := sqladmin.NewService(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("build Cloud SQL Admin client: %w", err)
	}
	redisSvc, err := redis.NewService(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("build Memorystore client: %w", err)
	}
	return sqladminClient{svc: sqlSvc}, memorystoreClient{svc: redisSvc}, nil
}

// azureClients resolves the PostgreSQL Flexible Server and Azure Managed Redis
// clients for a subscription via DefaultAzureCredential — the Azure analog of
// awsConfig's default-credential-chain and gcpClients' Application Default
// Credentials posture, with no static-credential branch. It returns one client
// per managed-service surface; each caller keeps the one it needs.
//
// subscriptionID CANNOT be inferred: DefaultAzureCredential yields a bearer
// token, never a subscription, so an empty one is refused here rather than
// producing an opaque ARM 404 on the first call.
//
// Like gcpClients and unlike awsConfig, NewDefaultAzureCredential resolves
// credentials eagerly. What keeps the provisioner constructors pure is PLACEMENT,
// not SDK behaviour: NewPostgresFlexibleProvisioner / NewManagedRedisProvisioner
// build no client, and this is called only from the lazy initClients inside
// Ensure. Do not hoist it into a constructor.
func azureClients(subscriptionID string) (azurePGAPI, azureRedisAPI, error) {
	if strings.TrimSpace(subscriptionID) == "" {
		return nil, nil, fmt.Errorf("managed data services on Azure need a subscription: pass --azure-subscription-id or set AZURE_SUBSCRIPTION_ID (Azure credentials yield a token, never a subscription, so it cannot be inferred)")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build Azure credential: %w", err)
	}
	servers, err := armpg.NewServersClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build PostgreSQL Flexible Server client: %w", err)
	}
	pgDatabases, err := armpg.NewDatabasesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build PostgreSQL Flexible Server databases client: %w", err)
	}
	pgConfigurations, err := armpg.NewConfigurationsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build PostgreSQL Flexible Server configurations client: %w", err)
	}
	clusters, err := armredis.NewClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build Azure Managed Redis client: %w", err)
	}
	redisDatabases, err := armredis.NewDatabasesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build Azure Managed Redis databases client: %w", err)
	}
	return azurePGClient{servers: servers, databases: pgDatabases, configurations: pgConfigurations},
		azureRedisClient{clusters: clusters, databases: redisDatabases},
		nil
}

// kubeClientset builds a typed Kubernetes clientset from the same getter Helm
// uses, for the credential Secret write.
func kubeClientset(cc ClientConfig, namespace string) (kubernetes.Interface, error) {
	getter := kube.RESTClientGetter(cc.Kubeconfig, namespace, cc.Context)
	return kube.NewClientset(getter)
}
