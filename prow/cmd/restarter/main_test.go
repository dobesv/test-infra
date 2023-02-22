/*
Copyright 2016 The Kubernetes Authors.

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
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxProwJobAge    = 2 * 24 * time.Hour
	maxPodAge        = 12 * time.Hour
	terminatedPodTTL = 30 * time.Minute // must be less than maxPodAge
)

func newDefaultFakeRestarterConfig() config.Restarter {
	return config.Restarter{
		MaxRestarts: 2,
	}
}

type fca struct {
	c *config.Config
}

func newFakeConfigAgent(s config.Restarter) *fca {
	return &fca{
		c: &config.Config{
			ProwConfig: config.ProwConfig{
				ProwJobNamespace: "ns",
				PodNamespace:     "ns",
				Restarter:        s,
			},
			JobConfig: config.JobConfig{
				Periodics: []config.Periodic{
					{JobBase: config.JobBase{Name: "retester"}},
				},
			},
		},
	}

}

func (f *fca) Config() *config.Config {
	return f.c
}

func startTime(s time.Time) *metav1.Time {
	start := metav1.NewTime(s)
	return &start
}

type unreachableCluster struct{ ctrlruntimeclient.Client }

func (unreachableCluster) Delete(_ context.Context, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.DeleteOption) error {
	return fmt.Errorf("I can't hear you.")
}

func (unreachableCluster) List(_ context.Context, _ ctrlruntimeclient.ObjectList, opts ...ctrlruntimeclient.ListOption) error {
	return fmt.Errorf("I can't hear you.")
}

func (unreachableCluster) Patch(_ context.Context, _ ctrlruntimeclient.Object, _ ctrlruntimeclient.Patch, _ ...ctrlruntimeclient.PatchOption) error {
	return errors.New("I can't hear you.")
}

func TestClean(t *testing.T) {
	//setComplete := func(d time.Duration) *metav1.Time {
	//	completed := metav1.NewTime(time.Now().Add(d))
	//	return &completed
	//}
	//prowJobs := []runtime.Object{
	//	&prowv1.ProwJob{
	//		ObjectMeta: metav1.ObjectMeta{
	//			Name:      "job-complete",
	//			Namespace: "ns",
	//		},
	//		Status: prowv1.ProwJobStatus{
	//			StartTime:      metav1.NewTime(time.Now().Add(-maxProwJobAge).Add(-time.Second)),
	//			CompletionTime: setComplete(-time.Second),
	//		},
	//	},
	//	&prowv1.ProwJob{
	//		ObjectMeta: metav1.ObjectMeta{
	//			Name:      "job-running",
	//			Namespace: "ns",
	//		},
	//		Status: prowv1.ProwJobStatus{
	//			StartTime: metav1.NewTime(time.Now().Add(-maxProwJobAge).Add(-time.Second)),
	//		},
	//	},
	//	&prowv1.ProwJob{
	//		ObjectMeta: metav1.ObjectMeta{
	//			Name:      "old-failed-normal",
	//			Namespace: "ns",
	//		},
	//		Status: prowv1.ProwJobStatus{
	//			StartTime:      metav1.NewTime(time.Now().Add(-maxProwJobAge).Add(-time.Second)),
	//			CompletionTime: setComplete(-time.Second),
	//		},
	//	},
	//	&prowv1.ProwJob{
	//		ObjectMeta: metav1.ObjectMeta{
	//			Name:      "old-deleted-unexpectedly",
	//			Namespace: "ns",
	//		},
	//		Status: prowv1.ProwJobStatus{
	//			StartTime:      metav1.NewTime(time.Now().Add(-maxProwJobAge).Add(-time.Second)),
	//			CompletionTime: setComplete(-time.Second),
	//			State:          prowv1.FailureState,
	//			Description:    "Pod got deleted unexpectedly",
	//		},
	//	},
	//}

	// TODO ... test something here
}

type clientWrapper struct {
	ctrlruntimeclient.Client
	getOnlyProwJobs map[string]*prowv1.ProwJob
}

func (c *clientWrapper) Get(ctx context.Context, key ctrlruntimeclient.ObjectKey, obj ctrlruntimeclient.Object) error {
	if pj, exists := c.getOnlyProwJobs[key.String()]; exists {
		*obj.(*prowv1.ProwJob) = *pj
		return nil
	}
	return c.Client.Get(ctx, key, obj)
}
