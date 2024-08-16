/*
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

package aws

import (
	"fmt"
	"context"
	"os"
	"testing"

	coretest "sigs.k8s.io/karpenter/pkg/test"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/client"
	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/aws/request"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	servicesqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite"
	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite/types"
	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/env"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/providers/sqs"
	"github.com/aws/karpenter-provider-aws/pkg/test"
	"github.com/aws/karpenter-provider-aws/test/pkg/environment/common"
)

func init() {
	karpv1.NormalizedLabels = lo.Assign(karpv1.NormalizedLabels, map[string]string{"topology.ebs.csi.aws.com/zone": corev1.LabelTopologyZone})
}

var WindowsDefaultImage = "mcr.microsoft.com/oss/kubernetes/pause:3.9"

var EphemeralInitContainerImage = "alpine"

type Environment struct {
	*common.Environment
	Region string

	STSAPI        *sts.STS
	EC2API        *ec2.EC2
	SSMAPI        *ssm.SSM
	IAMAPI        *iam.IAM
	FISAPI        *fis.FIS
	EKSAPI        *eks.EKS
	TimeStreamAPI TimestreamWriteAPI

	SQSProvider sqs.Provider

	ClusterName       string
	ClusterEndpoint   string
	InterruptionQueue string
	PrivateCluster    bool
	ZoneInfo          []ZoneInfo
}

type ZoneInfo struct {
	Zone     string
	ZoneID   string
	ZoneType string
}

func NewEnvironment(t *testing.T) *Environment {
	env := common.NewEnvironment(t)

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(os.Getenv("AWS_REGION")),
		config.WithSTSRegionalEndpoint(endpoints.RegionalSTSEndpoint),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *standard.RetryerOptions) {
				o.MaxAttempts = 10
			})
		}),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config, %v", err)
	}

	awsEnv := &Environment{
		Region:      cfg.Region,
		Environment: env,

		STSAPI:        sts.NewFromConfig(cfg),
		EC2API:        ec2.NewFromConfig(cfg),
		SSMAPI:        ssm.NewFromConfig(cfg),
		IAMAPI:        iam.NewFromConfig(cfg),
		FISAPI:        fis.NewFromConfig(cfg),
		EKSAPI:        eks.NewFromConfig(cfg),
		TimeStreamAPI: GetTimeStreamAPI(cfg),

		ClusterName:     lo.Must(os.LookupEnv("CLUSTER_NAME")),
		ClusterEndpoint: lo.Must(os.LookupEnv("CLUSTER_ENDPOINT")),
	}

	if _, awsEnv.PrivateCluster = os.LookupEnv("PRIVATE_CLUSTER"); awsEnv.PrivateCluster {
		WindowsDefaultImage = fmt.Sprintf("857221689048.dkr.ecr.%s.amazonaws.com/k8s/pause:3.6", awsEnv.Region)
		EphemeralInitContainerImage = fmt.Sprintf("857221689048.dkr.ecr.%s.amazonaws.com/ecr-public/docker/library/alpine:latest", awsEnv.Region)
		coretest.DefaultImage = fmt.Sprintf("857221689048.dkr.ecr.%s.amazonaws.com/ecr-public/eks-distro/kubernetes/pause:3.2", awsEnv.Region)
	}
	// Initialize the provider only if the INTERRUPTION_QUEUE environment variable is defined
	if v, ok := os.LookupEnv("INTERRUPTION_QUEUE"); ok {
		sqsapi := sqs.NewFromConfig(cfg)
		out := lo.Must(sqsapi.GetQueueUrl(env.Context, &servicesqs.GetQueueUrlInput{QueueName: aws.String(v)}))
		awsEnv.SQSProvider = lo.Must(sqs.NewDefaultProvider(sqsapi, lo.FromPtr(out.QueueUrl)))
	}
	// Populate ZoneInfo for all AZs in the region
	awsEnv.ZoneInfo = lo.Map(lo.Must(awsEnv.EC2API.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})).AvailabilityZones, func(zone *ec2.AvailabilityZone, _ int) ZoneInfo {
		return ZoneInfo{
			Zone:     lo.FromPtr(zone.ZoneName),
			ZoneID:   lo.FromPtr(zone.ZoneId),
			ZoneType: lo.FromPtr(zone.ZoneType),
		}
	})
	return awsEnv
}

func GetTimeStreamAPI(ctx context.Context, cfg aws.Config) timestreamwrite.Client {
	if lo.Must(env.GetBool("ENABLE_METRICS", false)) {
		By("enabling metrics firing for this suite")

		metricsRegion := env.GetString("METRICS_REGION", metricsDefaultRegion)
		timestreamCfg, err := config.LoadDefaultConfig(ctx, 
			config.WithRegion(metricsRegion),
		)
		if err != nil {
			return nil
		}
		return timestreamwrite.NewFromConfig(timestreamCfg)
	}
	return &NoOpTimeStreamAPI{}
}

func (n *NoOpTimeStreamAPI) WriteRecords(ctx context.Context, params *timestreamwrite.WriteRecordsInput, optFns ...func(*timestreamwrite.Options)) (*timestreamwrite.WriteRecordsOutput, error) {
	return &timestreamwrite.WriteRecordsOutput{}, nil
} 

func (env *Environment) DefaultEC2NodeClass() *v1.EC2NodeClass {
	nodeClass := test.EC2NodeClass()
	nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{Alias: "al2023@latest"}}
	nodeClass.Spec.Tags = map[string]string{
		"testing/cluster": env.ClusterName,
	}
	nodeClass.Spec.SecurityGroupSelectorTerms = []v1.SecurityGroupSelectorTerm{
		{
			Tags: map[string]string{"karpenter.sh/discovery": env.ClusterName},
		},
	}
	nodeClass.Spec.SubnetSelectorTerms = []v1.SubnetSelectorTerm{
		{
			Tags: map[string]string{"karpenter.sh/discovery": env.ClusterName},
		},
	}
	if env.PrivateCluster {
		nodeClass.Spec.Role = ""
		nodeClass.Spec.InstanceProfile = lo.ToPtr(fmt.Sprintf("KarpenterNodeInstanceProfile-%s", env.ClusterName))
		return nodeClass
	}
	nodeClass.Spec.Role = fmt.Sprintf("KarpenterNodeRole-%s", env.ClusterName)
	return nodeClass
}
