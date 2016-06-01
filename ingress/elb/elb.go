package elb

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	aws_elb "github.com/aws/aws-sdk-go/service/elb"
	"github.com/sky-uk/feed/ingress/api"
)

const (
	ElbTag      = "KubernetesClusterFrontend"
	maxTagQuery = 20
)

// New does something
func New(region string, clusterName string) api.Frontend {
	metadata := ec2metadata.New(session.New())
	log.Info("Is metadata availabe? ", metadata.Available())

	return &elb{
		metadata:    metadata,
		awsElb:      aws_elb.New(session.New(&aws.Config{Region: &region})),
		clusterName: clusterName,
	}
}

type elb struct {
	awsElb      ELB
	metadata    EC2Metadata
	clusterName string
}

// ELB interface to allow mocking of real calls to AWS as well as cutting down the methods from the real
// interface to only the ones we use
type ELB interface {
	DescribeLoadBalancers(input *aws_elb.DescribeLoadBalancersInput) (*aws_elb.DescribeLoadBalancersOutput, error)
	DescribeTags(input *aws_elb.DescribeTagsInput) (*aws_elb.DescribeTagsOutput, error)
	RegisterInstancesWithLoadBalancer(input *aws_elb.RegisterInstancesWithLoadBalancerInput) (*aws_elb.RegisterInstancesWithLoadBalancerOutput, error)
	DeregisterInstancesFromLoadBalancer(input *aws_elb.DeregisterInstancesFromLoadBalancerInput) (*aws_elb.DeregisterInstancesFromLoadBalancerOutput, error)
}

type EC2Metadata interface {
	Available() bool
	Region() (string, error)
	GetInstanceIdentityDocument() (ec2metadata.EC2InstanceIdentityDocument, error)
}

func (e *elb) Attach(frontend api.FrontendInput) (int, error) {
	log.Info("Attaching to loadbalancer with %v", frontend)

	id, err := e.metadata.GetInstanceIdentityDocument()
	if err != nil {
		return 0, fmt.Errorf("unable to query ec2 metadata service for InstanceId: %v", err)
	}

	instance := id.InstanceID
	log.Info("Ingress controller running on instance: ", instance)

	// Find the load balancers that are tagged with this cluster name
	pageSize := int64(400)
	request := &aws_elb.DescribeLoadBalancersInput{PageSize: &pageSize}
	var lbNames []*string
	for {
		resp, err := e.awsElb.DescribeLoadBalancers(request)

		if err != nil {
			return 0, fmt.Errorf("unable to describe load balancers %v", err)
		}

		for _, entry := range resp.LoadBalancerDescriptions {
			lbNames = append(lbNames, entry.LoadBalancerName)
		}

		if resp.NextMarker == nil {
			break
		}

		// Set the next marker
		request = &aws_elb.DescribeLoadBalancersInput{
			PageSize: &pageSize,
			Marker:   resp.NextMarker,
		}
	}

	var clusterFrontEnds []string
	totalLbs := len(lbNames)

	for i := 0; i < len(lbNames); i += maxTagQuery {
		to := min(i+maxTagQuery, totalLbs)
		log.Info(i, to)
		// Go slices are inclusive:exclusive
		names := lbNames[i:to]
		log.Info("Names", names)
		// TODO deal with error
		output, _ := e.awsElb.DescribeTags(&aws_elb.DescribeTagsInput{
			LoadBalancerNames: names,
		})

		for _, description := range output.TagDescriptions {
			for _, tag := range description.Tags {
				if *tag.Key == ElbTag && *tag.Value == e.clusterName {
					clusterFrontEnds = append(clusterFrontEnds, *description.LoadBalancerName)
				}
			}
		}
	}

	for _, frontend := range clusterFrontEnds {
		// TODO deal with error
		e.awsElb.RegisterInstancesWithLoadBalancer(&aws_elb.RegisterInstancesWithLoadBalancerInput{
			Instances: []*aws_elb.Instance{
				&aws_elb.Instance{
					InstanceId: aws.String(instance),
				}},
			LoadBalancerName: aws.String(frontend),
		})

	}

	return len(clusterFrontEnds), nil
}

func (e *elb) Detach(frontend api.FrontendInput) error {
	return nil
}

func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
