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
	"flag"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/pjutil/pprof"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/flagutil"
	configflagutil "k8s.io/test-infra/prow/flagutil/config"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	_ "k8s.io/test-infra/prow/version"
)

type options struct {
	runOnce                bool
	config                 configflagutil.ConfigOptions
	dryRun                 bool
	kubernetes             flagutil.KubernetesOptions
	instrumentationOptions flagutil.InstrumentationOptions
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	o := options{}
	fs.BoolVar(&o.runOnce, "run-once", false, "If true, run only once then quit.")

	fs.BoolVar(&o.dryRun, "dry-run", true, "Whether or not to make mutating API calls to Kubernetes.")

	o.config.AddFlags(fs)
	o.kubernetes.AddFlags(fs)
	o.instrumentationOptions.AddFlags(fs)
	fs.Parse(args)
	return o
}

func (o *options) Validate() error {
	if err := o.kubernetes.Validate(o.dryRun); err != nil {
		return err
	}

	if err := o.config.Validate(o.dryRun); err != nil {
		return err
	}

	return nil
}

func main() {
	logrusutil.ComponentInit()

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	defer interrupts.WaitForGracefulShutdown()

	pprof.Instrument(o.instrumentationOptions)

	configAgent, err := o.config.ConfigAgent()
	if err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	metrics.ExposeMetrics("restarter", cfg().PushGateway, o.instrumentationOptions.MetricsPort)

	ctrlruntimelog.SetLogger(zap.New(zap.JSONEncoder()))

	infrastructureClusterConfig, err := o.kubernetes.InfrastructureClusterConfig(o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting config for infastructure cluster")
	}

	// The watch apimachinery doesn't support restarts, so just exit the binary if a kubeconfig changes
	// to make the kubelet restart us.
	if err := o.kubernetes.AddKubeconfigChangeCallback(func() {
		logrus.Info("Kubeconfig changed, exiting to trigger a restart")
		interrupts.Terminate()
	}); err != nil {
		logrus.WithError(err).Fatal("Failed to register kubeconfig change callback")
	}

	opts := manager.Options{
		MetricsBindAddress:            "0",
		Namespace:                     cfg().ProwJobNamespace,
		LeaderElection:                true,
		LeaderElectionNamespace:       configAgent.Config().ProwJobNamespace,
		LeaderElectionID:              "prow-restarter-leaderlock",
		LeaderElectionReleaseOnCancel: true,
	}
	mgr, err := manager.New(infrastructureClusterConfig, opts)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating manager")
	}

	c := controller{
		ctx:           context.Background(),
		logger:        logrus.NewEntry(logrus.StandardLogger()),
		prowJobClient: mgr.GetClient(),
		config:        cfg,
		runOnce:       o.runOnce,
		dryRun:        o.dryRun,
	}
	if err := mgr.Add(&c); err != nil {
		logrus.WithError(err).Fatal("failed to add controller to manager")
	}
	if err := mgr.Start(interrupts.Context()); err != nil {
		logrus.WithError(err).Fatal("failed to start manager")
	}
	logrus.Info("Manager ended gracefully")
}

type controller struct {
	ctx           context.Context
	logger        *logrus.Entry
	prowJobClient ctrlruntimeclient.Client
	config        config.Getter
	runOnce       bool
	dryRun        bool
}

func (c *controller) Start(ctx context.Context) error {
	runChan := make(chan struct{})

	// We want to be able to dynamically adjust to changed config values, hence we cant use a time.Ticker
	go func() {
		for {
			runChan <- struct{}{}
			time.Sleep(c.config().Restarter.ResyncPeriod.Duration)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("stop signal received, quitting")
			return nil
		case <-runChan:
			start := time.Now()
			c.run()
			c.logger.Infof("Sync time: %v", time.Since(start))
			if c.runOnce {
				return nil
			}
		}
	}
}

type restarterReconciliationMetrics struct {
	startAt               time.Time
	finishedAt            time.Time
	prowJobsCreated       int
	prowJobsRestarted     int
	prowJobsRestartErrors int
}

// Prometheus Metrics
var (
	restarterMetrics = struct {
		timeUsed              prometheus.Gauge
		prowJobsCreated       prometheus.Gauge
		prowJobsRestarted     prometheus.Gauge
		prowJobsRestartErrors prometheus.Gauge
	}{
		timeUsed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "restarter_loop_duration_seconds",
			Help: "Time used in each reconciliation.",
		}),
		prowJobsCreated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "restarter_prow_jobs_existing",
			Help: "Number of the existing prow jobs in each reconciliation.",
		}),
		prowJobsRestarted: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "restarter_prow_jobs_restarted",
			Help: "Number of prow jobs restarted in each reconciliation.",
		}), prowJobsRestartErrors: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "restarter_prow_jobs_restart_errors",
			Help: "Number of prow jobs failed to be restarted in each reconciliation.",
		}),
	}
)

func init() {
	prometheus.MustRegister(restarterMetrics.timeUsed)
	prometheus.MustRegister(restarterMetrics.prowJobsCreated)
	prometheus.MustRegister(restarterMetrics.prowJobsRestarted)
	prometheus.MustRegister(restarterMetrics.prowJobsRestartErrors)
}

func (m *restarterReconciliationMetrics) getTimeUsed() time.Duration {
	return m.finishedAt.Sub(m.startAt)
}

func (c *controller) run() {

	metrics := restarterReconciliationMetrics{
		startAt:               time.Now(),
		prowJobsRestarted:     0,
		prowJobsRestartErrors: 0,
	}

	// List prow jobs
	prowJobs := &prowv1.ProwJobList{}
	if err := c.prowJobClient.List(c.ctx, prowJobs, ctrlruntimeclient.InNamespace(c.config().ProwJobNamespace)); err != nil {
		c.logger.WithError(err).Error("Error listing prow jobs.")
		return
	}
	metrics.prowJobsCreated = len(prowJobs.Items)
	maxRestarts := c.config().Restarter.MaxRestarts
	for _, prowJob := range prowJobs.Items {
		prevRestartsStr, prevRestartsSet := prowJob.ObjectMeta.Labels[kube.RestartCountLabel]
		prevRestartsCount := 0
		if prevRestartsSet && prevRestartsStr != "" {
			parsedRestartsCount, err := strconv.Atoi(prevRestartsStr)
			if err != nil {
				c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error parsing job restart count, skipping")
				continue
			}
			prevRestartsCount = parsedRestartsCount
		}
		if prevRestartsCount >= maxRestarts {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("job has reached maximum number of restarts, skipping")
			continue
		}

		c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithField("statusDescription", prowJob.Status.Description).WithField("prevRestarts", prevRestartsStr).Debug("Checking prowjob.")

		wantToRestart := ((prowJob.Status.State == prowv1.FailureState || prowJob.Status.State == prowv1.ErrorState) && prowJob.Status.Description == "Pod got deleted unexpectedly") ||
			(prowJob.Status.State == prowv1.ErrorState && prowJob.Status.Description == "Pod scheduling timeout.")

		if !wantToRestart {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("job status does not indicate a restart")
			continue
		}

		originalJobName := prowJob.ObjectMeta.Name
		if label, isSet := prowJob.ObjectMeta.Labels[kube.OriginalJobName]; isSet {
			originalJobName = label
		}

		// See if there's another job that would already be considered a re-run of this job
		newerJobExists := false
		for _, otherProwJob := range prowJobs.Items {
			if otherProwJob.CreationTimestamp.Time.After(prowJob.CreationTimestamp.Time) {
				// Check if the other job has the same original job
				if value, isSet := otherProwJob.ObjectMeta.Labels[kube.OriginalJobName]; isSet && originalJobName == value {
					newerJobExists = true
					break
				}

				// Check if labels that describe what is built by the job are matching
				allEqual := true
				for _, k := range []string{
					kube.ContextAnnotation,
					kube.ProwJobTypeLabel,
					kube.ProwBuildIDLabel,
					kube.ProwJobAnnotation,
					kube.OrgLabel,
					kube.RepoLabel,
					kube.BaseRefLabel,
					kube.PullLabel,
				} {
					if prowJob.ObjectMeta.Labels[k] != otherProwJob.ObjectMeta.Labels[k] {
						allEqual = false
						break
					}
				}
				if allEqual {
					newerJobExists = true
					break
				}
			}
		}
		if newerJobExists {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Not restarting job as there is already a newer similar job")
			continue
		}

		// Looks like we need to restart this job
		newProwJob := pjutil.NewProwJob(prowJob.Spec, prowJob.ObjectMeta.Labels, prowJob.ObjectMeta.Annotations)
		newProwJob.ObjectMeta.Labels[kube.CreatedByRestarterLabel] = "true"
		newProwJob.ObjectMeta.Labels[kube.RestartCountLabel] = strconv.Itoa(prevRestartsCount + 1)

		newProwJob.ObjectMeta.Labels[kube.OriginalJobName] = originalJobName

		newProwJob.ObjectMeta.Namespace = prowJob.ObjectMeta.Namespace
		if err := c.prowJobClient.Create(context.TODO(), &newProwJob); err != nil {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error restarting prowjob.")
			metrics.prowJobsRestartErrors++
		} else {
			c.logger.WithFields(pjutil.ProwJobFields(&newProwJob)).WithField("originalJobName", originalJobName).Info("Created new prowjob to retry failed job.")
			metrics.prowJobsRestarted++
		}
	}

	metrics.finishedAt = time.Now()
	restarterMetrics.timeUsed.Set(float64(metrics.getTimeUsed().Seconds()))
	restarterMetrics.prowJobsCreated.Set(float64(metrics.prowJobsCreated))
	restarterMetrics.prowJobsRestarted.Set(float64(metrics.prowJobsRestarted))
	restarterMetrics.prowJobsRestartErrors.Set(float64(metrics.prowJobsRestartErrors))
	c.logger.Info("Restarter reconciliation complete.")
}
