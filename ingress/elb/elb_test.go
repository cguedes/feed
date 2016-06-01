package elb

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	aws_elb "github.com/aws/aws-sdk-go/service/elb"
	"github.com/sky-uk/feed/ingress/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"testing"
	"fmt"
	"errors"
)

const (
	clusterName = "cluster_name"
)

type fakeElb struct {
	mock.Mock
}

func (m *fakeElb) DescribeLoadBalancers(input *aws_elb.DescribeLoadBalancersInput) (*aws_elb.DescribeLoadBalancersOutput, error) {
	args := m.Called(input)
	return args.Get(0).(*aws_elb.DescribeLoadBalancersOutput), args.Error(1)
}

func (m *fakeElb) DescribeTags(input *aws_elb.DescribeTagsInput) (*aws_elb.DescribeTagsOutput, error) {
	args := m.Called(input)
	return args.Get(0).(*aws_elb.DescribeTagsOutput), nil
}

func (m *fakeElb) DeregisterInstancesFromLoadBalancer(input *aws_elb.DeregisterInstancesFromLoadBalancerInput) (*aws_elb.DeregisterInstancesFromLoadBalancerOutput, error) {
	args := m.Called(input)
	return args.Get(0).(*aws_elb.DeregisterInstancesFromLoadBalancerOutput), nil
}

func (m *fakeElb) RegisterInstancesWithLoadBalancer(input *aws_elb.RegisterInstancesWithLoadBalancerInput) (*aws_elb.RegisterInstancesWithLoadBalancerOutput, error) {
	args := m.Called(input)
	return args.Get(0).(*aws_elb.RegisterInstancesWithLoadBalancerOutput), nil
}

type fakeMetadata struct {
	mock.Mock
}

func (m *fakeMetadata) Available() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *fakeMetadata) Region() (string, error) {
	args := m.Called()
	return args.String(0), nil
}

func (m *fakeMetadata) GetInstanceIdentityDocument() (ec2metadata.EC2InstanceIdentityDocument, error) {
	args := m.Called()
	return args.Get(0).(ec2metadata.EC2InstanceIdentityDocument), args.Error(1)
}

func mockLoadBalancers(m *fakeElb, lbs ...string) {
	var descriptions []*aws_elb.LoadBalancerDescription
	for _, lb := range lbs {
		descriptions = append(descriptions, &aws_elb.LoadBalancerDescription{
			LoadBalancerName: aws.String(lb),
		})

	}
	m.On("DescribeLoadBalancers", mock.AnythingOfType("*elb.DescribeLoadBalancersInput")).Return(&aws_elb.DescribeLoadBalancersOutput{
		LoadBalancerDescriptions: descriptions,
	}, nil)
}

type lbTags struct {
	tags []*aws_elb.Tag
	name string
}

func mockClusterTags(m *fakeElb, lbs ...lbTags) {
	var tagDescriptions []*aws_elb.TagDescription

	for _, lb := range lbs {
		tagDescriptions = append(tagDescriptions, &aws_elb.TagDescription{
			LoadBalancerName: aws.String(lb.name),
			Tags: lb.tags,
		})
	}

	m.On("DescribeTags", mock.AnythingOfType("*elb.DescribeTagsInput")).Return(&aws_elb.DescribeTagsOutput{
		TagDescriptions: tagDescriptions,
	}, nil)
}

func setup() (api.Frontend, *fakeElb, *fakeMetadata) {
	e := New("eu-west-1", clusterName)
	mockElb := &fakeElb{}
	mockMetadata := &fakeMetadata{}
	e.(*elb).awsElb = mockElb
	e.(*elb).metadata = mockMetadata
	return e, mockElb, mockMetadata
}

func TestAttachWithSingleMatchingLoadBalancers(t *testing.T) {
	// given
	e, mockElb, mockMetadata := setup()
	instanceId := "cow"
	mockMetadata.On("GetInstanceIdentityDocument").Return(ec2metadata.EC2InstanceIdentityDocument{InstanceID: instanceId}, nil)
	clusterFrontEnd := "cluster-frontend"
	clusterFrontEndDifferentCluster := "cluster-frontend-different-cluster"
	mockLoadBalancers(mockElb, clusterFrontEnd, clusterFrontEndDifferentCluster, "other")
	mockClusterTags(mockElb,
		lbTags{ name: clusterFrontEnd, tags: []*aws_elb.Tag { &aws_elb.Tag{ Key: aws.String("KubernetesClusterFrontend"), Value: aws.String(clusterName) }, }},
		lbTags{ name: clusterFrontEndDifferentCluster, tags: []*aws_elb.Tag { &aws_elb.Tag{ Key: aws.String("KubernetesClusterFrontend"), Value: aws.String("different cluster") }, }},
		lbTags{ name: "other elb", tags: []*aws_elb.Tag { &aws_elb.Tag{ Key: aws.String("Bannana"), Value: aws.String("Tasty") }, }},
	)
	mockElb.On("RegisterInstancesWithLoadBalancer", &aws_elb.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: aws.String(clusterFrontEnd),
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	}).Return(&aws_elb.RegisterInstancesWithLoadBalancerOutput{
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	})

	//when
	number, err := e.Attach(api.FrontendInput{
		Cluster: "test",
	})

	//then
	assert.Equal(t, 1, number)
	mockElb.AssertExpectations(t)
	mockMetadata.AssertExpectations(t)
	assert.NoError(t, err)
}

func TestAttachWithMultipleMatchingLoadBalancers(t *testing.T) {
	// given
	e, mockElb, mockMetadata := setup()
	instanceId := "cow"
	mockMetadata.On("GetInstanceIdentityDocument").Return(ec2metadata.EC2InstanceIdentityDocument{InstanceID: instanceId}, nil)
	clusterFrontEnd := "cluster-frontend"
	clusterFrontEnd2 := "cluster-frontend2"
	mockLoadBalancers(mockElb, clusterFrontEnd, clusterFrontEnd2)
	mockClusterTags(mockElb,
		lbTags{ name: clusterFrontEnd, tags: []*aws_elb.Tag { &aws_elb.Tag{ Key: aws.String("KubernetesClusterFrontend"), Value: aws.String(clusterName) }, }},
		lbTags{ name: clusterFrontEnd2, tags: []*aws_elb.Tag { &aws_elb.Tag{ Key: aws.String("KubernetesClusterFrontend"), Value: aws.String(clusterName) }, }},
	)
	mockElb.On("RegisterInstancesWithLoadBalancer", &aws_elb.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: aws.String(clusterFrontEnd),
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	}).Return(&aws_elb.RegisterInstancesWithLoadBalancerOutput{
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	})
	mockElb.On("RegisterInstancesWithLoadBalancer", &aws_elb.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: aws.String(clusterFrontEnd2),
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	}).Return(&aws_elb.RegisterInstancesWithLoadBalancerOutput{
		Instances: []*aws_elb.Instance{ &aws_elb.Instance{InstanceId: aws.String(instanceId)} },
	})

	//when
	number, err := e.Attach(api.FrontendInput{
		Cluster: "test",
	})

	//then
	assert.Equal(t, 2, number)
	mockElb.AssertExpectations(t)
	mockMetadata.AssertExpectations(t)
	assert.NoError(t, err)
}


func TestErrorGettingMetadata(t *testing.T) {
	e, _, mockMetadata := setup()
	mockMetadata.On("GetInstanceIdentityDocument").Return(ec2metadata.EC2InstanceIdentityDocument{}, fmt.Errorf("No metadata for you"))

	_, err := e.Attach(api.FrontendInput{
		Cluster: "test",
	})

	assert.EqualError(t, err, "Unable to query ec2 metadata service for InstanceId: No metadata for you")
}

func TestErrorDescribingInstances(t *testing.T) {
	e, mockElb, mockMetadata := setup()
	instanceId := "cow"
	mockMetadata.On("GetInstanceIdentityDocument").Return(ec2metadata.EC2InstanceIdentityDocument{InstanceID: instanceId}, nil)
	mockElb.On("DescribeLoadBalancers", mock.AnythingOfType("*elb.DescribeLoadBalancersInput")).Return(nil, errors.New("Oh dear oh dear"))

	_, err := e.Attach(api.FrontendInput{
		Cluster: "test",
	})

	assert.EqualError(t, err, "Pants");
	assert.Equal(t, 1, 2)
}

func TestErrorDescribingTags(t *testing.T) {
	assert.Equal(t, 1, 2)
}

func TestNoMatchingElbs(t *testing.T) {
	assert.Equal(t, 1, 2)
}
// Test the paging for load balancers
// Test calls to get tags paging
