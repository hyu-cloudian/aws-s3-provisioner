/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"flag"
	"fmt"
	_ "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	awsuser "github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	"github.com/golang/glog"
	"github.com/yard-turkey/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	libbkt "github.com/yard-turkey/lib-bucket-provisioner/pkg/provisioner"
	apibkt "github.com/yard-turkey/lib-bucket-provisioner/pkg/provisioner/api"
	bkterr "github.com/yard-turkey/lib-bucket-provisioner/pkg/provisioner/api/errors"

	storageV1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/aws-sdk-go/aws/credentials"
)

const (
	defaultRegion    = "us-west-1"
	httpPort         = 80
	httpsPort        = 443
	provisionerName  = "aws-s3.io/bucket"
	regionInsert     = "<REGION>"
	s3Hostname       = "s3-" + regionInsert + ".amazonaws.com"
	s3BucketArn      = "arn:aws:s3:::%s"
	policyArn        = "arn:aws:iam::%s:policy/%s"
	createBucketUser = false
	obStateARN	 = "ARN"
)

var (
	kubeconfig string
	masterURL  string
)

type awsS3Provisioner struct {
	bucketName string
	region     string
	// session is the aws session
	session	   *session.Session
	// s3svc is the aws s3 service based on the session
	s3svc	   *s3.S3
	// iam client service
	iamsvc	   *awsuser.IAM
	//kube client
	clientset  *kubernetes.Clientset
	// access keys for aws acct for the bucket *owner*
	bktOwnerAccessId  string
	bktOwnerSecretKey string
	bktCreateUser     string
	bktUserName       string
	bktUserAccessId   string
	bktUserSecretKey  string
	bktUserAccountId  string
	bktUserPolicyArn  string
}

func NewAwsS3Provisioner(cfg *restclient.Config, s3Provisioner awsS3Provisioner) *libbkt.ProvisionerController {

	libCfg := &libbkt.Config{}
	return libbkt.NewProvisioner(cfg, provisionerName, s3Provisioner, libCfg)
}

// Return the aws default session.
func awsDefaultSession() (*session.Session, error) {

	glog.V(2).Infof("Creating AWS *default* session")
	return session.NewSession(&aws.Config{
		Region: aws.String(defaultRegion),
		//Credentials: credentials.NewStaticCredentials(os.Getenv),
	})
}

// Return the OB struct with minimal fields filled in.
func (p *awsS3Provisioner) rtnObjectBkt(name string) *v1alpha1.ObjectBucket {

	host := strings.Replace(s3Hostname, regionInsert, p.region, 1)
//ep := v1alpha1.Endpoint{
		//BucketHost: host,
		//BucketPort: httpsPort,
		//BucketName: name,
		//Region:     p.region,
		//SSL:        true,
//}

//auth := v1alpha1.Authentication{
		//AccessKeys: &v1alpha1.AccessKeys{
			//AccessKeyId:     p.bktUserAccessId,
			//SecretAccessKey: p.bktUserSecretKey,
		//},
//}
//conn := &v1alpha1.Connection{
	//Endpoint: &ep,
	//Authentication: &auth,
	//AdditionalState: map[string]string{
		//obStateARN: p.bktUserPolicyArn,
	//},
//}
//glog.Infof("=====DEBUG====* ep=%+v, auth=%+v, conn=%+v", ep, auth, conn)

	conn := &v1alpha1.Connection{
		Endpoint: &v1alpha1.Endpoint{
			BucketHost: host,
			BucketPort: httpsPort,
			BucketName: name,
			Region:     p.region,
			SSL:        true,
		},
		Authentication: &v1alpha1.Authentication{
			AccessKeys: &v1alpha1.AccessKeys{
				AccessKeyId:     p.bktUserAccessId,
				SecretAccessKey: p.bktUserSecretKey,
			},
		},
		AdditionalState: map[string]string{
			obStateARN: p.bktUserPolicyArn,
		},
	}
//glog.Infof("=====DEBUG====* host=%q, conn=%+v", host, conn)

	return &v1alpha1.ObjectBucket{
		//TypeMeta: metav1.TypeMeta{
			//Kind: "ObjectBucket",
			//APIVersion: "v1alpha1",
		//},
		//ObjectMeta: metav1.ObjectMeta{
			//Name: "jeff-debugging-ob",
		//},
		Spec: v1alpha1.ObjectBucketSpec{
			Connection: conn,
		},
	}
}

func (p *awsS3Provisioner) createBucket(name string) error {

	bucketinput := &s3.CreateBucketInput{
		Bucket: &name,
	}

	_, err := p.s3svc.CreateBucket(bucketinput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyExists:
				msg := fmt.Sprintf("Bucket %q already exists", name)
				glog.Errorf(msg)
				return bkterr.NewBucketExistsError(msg)
			case s3.ErrCodeBucketAlreadyOwnedByYou:
				msg := fmt.Sprintf("Bucket %q already owned by you", name)
				glog.Errorf(msg)
				return bkterr.NewBucketExistsError(msg)
			}
		}
		return fmt.Errorf("Bucket %q could not be created: %v", name, err)
	}
	glog.Infof("Bucket %s successfully created", name)

	//Now at this point, we have a bucket and an owner
	//we should now create the user for the bucket
	return nil
}

// Create an aws session based on the OBC's storage class's secret and region.
// Set in the receiver the session and region used to create the session.
// Note: in error cases it's possible that the set region is different from
//   the OBC's storage class's region.
func (p *awsS3Provisioner) awsSessionFromStorageClass(sc *storageV1.StorageClass) error {

	const scRegionKey = "region"

	// helper func var for error returns
	var errDefault = func() error {
		var err error
		p.region = defaultRegion
		p.session, err = awsDefaultSession()
		return err
	}

	// get expected sc parameters containing region and secret
	region, ok := sc.Parameters[scRegionKey]
	if !ok || region == "" {
		region = defaultRegion
	}
	secretNS, secretName := getSecretName(sc.Parameters)
	if secretNS == "" || secretName == "" {
		glog.Warningf("secret name or namespace are empty in storage class %q", sc.Name)
		return errDefault()
	}

	// get the sc's bucket owner secret
	err := p.credsFromSecret(p.clientset, secretNS, secretName)
	if err != nil {
		glog.Warningf("secret \"%s/%s\" in storage class %q for %q is empty.\nUsing default credentials.", secretNS, secretName, sc.Name, p.bucketName)
		return errDefault()
	}

	// use the OBC's SC to create our session, set receiver fields
	glog.V(2).Infof("Creating aws session using credentials from storage class %s's secret", sc.Name)
	p.region = region
	p.session, err = session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Credentials: credentials.NewStaticCredentials(p.bktOwnerAccessId, p.bktOwnerSecretKey, ""),
	})

	return err
}

// Provision creates an aws s3 bucket and returns a connection info
// representing the bucket's endpoint and user access credentials.
// Programming Note: _all_ methods on "awsS3Provisioner" called directly
//   or indirectly by `Provision` should use pointer receivers. This allows
//   all supporting methods to set receiver fields where convenient. An
//   alternative (arguably better) would be for all supporting methods
//   to take value receivers and functionally return back to `Provision`
//   receiver fields they need set. The first approach is easier for now.
func (p awsS3Provisioner) Provision(options *apibkt.BucketOptions) (*v1alpha1.ObjectBucket, error) {

	// set the bucket name
	p.bucketName = options.BucketName

	// get the OBC and its storage class
	obc := options.ObjectBucketClaim
	scName := options.ObjectBucketClaim.Spec.StorageClassName
	//sc, err := p.getClassForBucketClaim(obc)
	sc, err := p.getClassByNameForBucket(scName)
	if err != nil {
		glog.Errorf("failed to get storage class %q for OBC %q: %v", obc.Spec.StorageClassName, obc.Name, err)
		return nil, err
	}

	// check for bkt user access policy vs. bkt owner policy based on SC
	p.setCreateBucketUserOptions(obc, sc)

	// set the aws session from the obc in the receiver
	err = p.awsSessionFromStorageClass(sc)
	if err != nil {
		return nil, fmt.Errorf("error creating session from OBC \"%s/%s\": %v", obc.Namespace, obc.Name, err)
	}

	glog.V(2).Infof("Creating S3 service for OBC \"%s/%s\"", obc.Namespace, obc.Name)
	p.s3svc = s3.New(p.session)

	// create the bucket
	glog.Infof("Creating bucket %q", p.bucketName)
	err = p.createBucket(p.bucketName)
	if err != nil {
		err = fmt.Errorf("error creating bucket %q: %v", p.bucketName, err)
		glog.Errorf(err.Error())
		return nil, err
	}

	// createBucket was successful, deal with user and policy
	// TODO: default access and key are set to bkt owner.
	//   This needs to be more restrictive or a failure...
	p.bktUserAccessId = p.bktOwnerAccessId
	p.bktUserSecretKey = p.bktOwnerSecretKey
	if p.bktCreateUser == "yes" {
		// Create a new IAM user using the name of the bucket and set
		// access and attach policy for bucket and user
		p.bktUserName = p.bucketName

		// handle all iam and policy operations
		uAccess, uKey, err := p.handleUserAndPolicy(options)
		if err != nil || uAccess == "" || uKey == "" {
			//what to do - something failed along the way
			//do we fall back and create our connection with
			//the default bktOwnerRef?
			glog.Errorf("Something failed along the way for handling Users and Policy: %v", err)
		} else {
			p.bktUserAccessId = uAccess
			p.bktUserSecretKey = uKey
		}
	}

	// returned connection info
	return p.rtnObjectBkt(p.bucketName), nil
}

// Delete the bucket and all its objects.
// Note: only called when the bucket's reclaim policy is "delete".
func (p awsS3Provisioner) Delete(ob *v1alpha1.ObjectBucket) error {

	// set receiver fields from OB data
	p.bucketName = ob.Spec.Endpoint.BucketName
	p.bktUserPolicyArn = ob.Spec.AdditionalState[obStateARN]
	scName := ob.Spec.StorageClassName
	glog.Infof("Deleting bucket %q for OB %q", p.bucketName, ob.Name)


	// get the OB and its storage class
	sc, err := p.getClassByNameForBucket(scName)
	if err != nil {
		glog.Errorf("failed to get storage class %q for OB %q: %v", ob.Spec.StorageClassName, ob.Name, err)
		return err
	}

	// set the aws session
	err = p.awsSessionFromStorageClass(sc)
	if err != nil {
		return fmt.Errorf("error creating session from OB \"%s/%s\": %v", ob.Namespace, ob.Name, err)
	}

	glog.V(2).Infof("Creating S3 service for OB \"%s/%s\"", ob.Namespace, ob.Name)
	p.s3svc = s3.New(p.session)


	// Delete IAM Policy and User
	erruser := p.handleUserAndPolicyDeletion(p.bucketName)
	if erruser != nil {
		// We are currently only logging
		// because if failure do not want to stop
		// deletion of bucket
		glog.Infof("Failed to delete Policy and/or User - manual clean up required")
		//return fmt.Errorf("Error deleting Policy and/or User %v", err)
	}

	// Delete Bucket
	iter := s3manager.NewDeleteListIterator(p.s3svc, &s3.ListObjectsInput{
		Bucket: aws.String(p.bucketName),
	})

	glog.V(2).Infof("Deleting all objects in bucket %q (from OB %q)", p.bucketName, ob.Name)
	err = s3manager.NewBatchDeleteWithClient(p.s3svc).Delete(aws.BackgroundContext(), iter)
	if err != nil {
		return fmt.Errorf("Error deleting objects from bucket %q: %v", p.bucketName, err)
	}

	glog.V(2).Infof("Deleting empty bucket %q from OB %q", p.bucketName, ob.Name)
	_, err = p.s3svc.DeleteBucket(&s3.DeleteBucketInput{
		Bucket: aws.String(p.bucketName),
	})
	if err != nil {
		return fmt.Errorf("Error deleting empty bucket %q: %v", p.bucketName, err)
	}
	glog.Infof("Deleted bucket %q from OB %q", p.bucketName, ob.Name)



	return nil

}

// create k8s config and client for the runtime-controller.
// Note: panics on errors.
func createConfigAndClientOrDie(masterurl, kubeconfig string) (*restclient.Config, *kubernetes.Clientset) {
	config, err := clientcmd.BuildConfigFromFlags(masterurl, kubeconfig)
	if err != nil {
		glog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}
	return config, clientset
}

func main() {
	defer glog.Flush()
	syscall.Umask(0)

	handleFlags()

	glog.Infof("AWS S3 Provisioner - main")
	glog.V(2).Infof("flags: kubeconfig=%q; masterURL=%q", kubeconfig, masterURL)

	config, clientset := createConfigAndClientOrDie(masterURL, kubeconfig)

	stopCh := handleSignals()

	s3Prov := awsS3Provisioner{}
	s3Prov.clientset = clientset

	// Create and run the s3 provisioner controller.
	// It implements the Provisioner interface expected by the bucket
	// provisioning lib.
	S3ProvisionerController := NewAwsS3Provisioner(config, s3Prov)
	glog.V(2).Infof("main: running %s provisioner...", provisionerName)
	S3ProvisionerController.Run()

	<-stopCh
	glog.Infof("main: %s provisioner exited.", provisionerName)
}

// --kubeconfig and --master are set in the controller-runtime's config
// package's init(). Set global kubeconfig and masterURL variables depending
// on flag values or env variables.
// Note: `alsologtostderr` *must* be specified on the command line to see
//   provisioner and bucket library logging. Setting it here does not affect
//   the lib because its init() function has already run.
func handleFlags() {

	if !flag.Parsed() {
		flag.Parse()
	}

	flag.VisitAll(func(f *flag.Flag) {
		if f.Name == "kubeconfig" {
			kubeconfig = flag.Lookup(f.Name).Value.String()
			if kubeconfig == "" {
				kubeconfig = os.Getenv("KUBECONFIG")
			}
			return
		}
		if f.Name == "master" {
			masterURL = flag.Lookup(f.Name).Value.String()
			if masterURL == "" {
				masterURL = os.Getenv("MASTER")
			}
			return
		}
	})
}

// Shutdown gracefully on system signals.
func handleSignals() <-chan struct{} {
	sigCh := make(chan os.Signal)
	stopCh := make(chan struct{})
	go func() {
		signal.Notify(sigCh)
		<-sigCh
		close(stopCh)
		os.Exit(1)
	}()
	return stopCh
}
