/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication

import (
	"fmt"
	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/apis/sparkoperator.k8s.io/v1beta1"
	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/config"
)

const (
	metricsPropertiesKey          = "metrics.properties"
	prometheusConfigKey           = "prometheus.yaml"
	prometheusConfigMapNameSuffix = "prom-conf"
	prometheusConfigMapMountPath  = "/etc/metrics/conf"
	prometheusScrapeAnnotation    = "prometheus.io/scrape"
	prometheusPortAnnotation      = "prometheus.io/port"
	prometheusPathAnnotation      = "prometheus.io/path"
)

func configPrometheusMonitoring(app *v1beta1.SparkApplication, kubeClient clientset.Interface) error {
	port := config.DefaultPrometheusJavaAgentPort
	if app.Spec.Monitoring.Prometheus.Port != nil {
		port = *app.Spec.Monitoring.Prometheus.Port
	}

	configFile := app.Spec.Monitoring.Prometheus.ConfigFile
	var javaOption string
	if configFile != "" {
		glog.V(2).Infof("Overriding the default Prometheus configuration with config file %s in the Spark job image.", configFile)
		javaOption = fmt.Sprintf("-javaagent:%s=%d:%s", app.Spec.Monitoring.Prometheus.JmxExporterJar,
			port, configFile)
	} else {
		glog.V(2).Infof("Using the default Prometheus configuration.")
		prometheusConfigMapName := fmt.Sprintf("%s-%s", app.Name, prometheusConfigMapNameSuffix)
		configMap := buildPrometheusConfigMap(app, prometheusConfigMapName)
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cm, err := kubeClient.CoreV1().ConfigMaps(app.Namespace).Get(prometheusConfigMapName, metav1.GetOptions{})

			if apiErrors.IsNotFound(err) {
				_, createErr := kubeClient.CoreV1().ConfigMaps(app.Namespace).Create(configMap)
				return createErr
			}
			if err != nil {
				return err
			}

			cm.Data = configMap.Data
			_, updateErr := kubeClient.CoreV1().ConfigMaps(app.Namespace).Update(cm)
			return updateErr
		})

		if retryErr != nil {
			return fmt.Errorf("failed to apply %s in namespace %s: %v", prometheusConfigMapName, app.Namespace, retryErr)
		}

		javaOption = fmt.Sprintf("-javaagent:%s=%d:%s/%s", app.Spec.Monitoring.Prometheus.JmxExporterJar,
			port, prometheusConfigMapMountPath, prometheusConfigKey)

		if app.Spec.Monitoring.ExposeDriverMetrics {
			app.Spec.Driver.ConfigMaps = append(app.Spec.Driver.ConfigMaps, v1beta1.NamePath{
				Name: prometheusConfigMapName,
				Path: prometheusConfigMapMountPath,
			})
		}
		if app.Spec.Monitoring.ExposeExecutorMetrics {
			app.Spec.Executor.ConfigMaps = append(app.Spec.Executor.ConfigMaps, v1beta1.NamePath{
				Name: prometheusConfigMapName,
				Path: prometheusConfigMapMountPath,
			})
		}
	}

	/* work around for push gateway issue: https://github.com/prometheus/pushgateway/issues/97 */
	metricNamespace := fmt.Sprintf("%s.%s", app.Namespace, app.Name)
	metricConf := fmt.Sprintf("%s/%s", prometheusConfigMapMountPath, metricsPropertiesKey)
	if app.Spec.SparkConf == nil {
		app.Spec.SparkConf = make(map[string]string)
	}
	app.Spec.SparkConf["spark.metrics.namespace"] = metricNamespace
	app.Spec.SparkConf["spark.metrics.conf"] = metricConf

	if app.Spec.Monitoring.ExposeDriverMetrics {
		if app.Spec.Driver.Annotations == nil {
			app.Spec.Driver.Annotations = make(map[string]string)
		}
		app.Spec.Driver.Annotations[prometheusScrapeAnnotation] = "true"
		app.Spec.Driver.Annotations[prometheusPortAnnotation] = fmt.Sprintf("%d", port)
		app.Spec.Driver.Annotations[prometheusPathAnnotation] = "/metrics"

		if app.Spec.Driver.JavaOptions == nil {
			app.Spec.Driver.JavaOptions = &javaOption
		} else {
			*app.Spec.Driver.JavaOptions = *app.Spec.Driver.JavaOptions + " " + javaOption
		}
	}
	if app.Spec.Monitoring.ExposeExecutorMetrics {
		if app.Spec.Executor.Annotations == nil {
			app.Spec.Executor.Annotations = make(map[string]string)
		}
		app.Spec.Executor.Annotations[prometheusScrapeAnnotation] = "true"
		app.Spec.Executor.Annotations[prometheusPortAnnotation] = fmt.Sprintf("%d", port)
		app.Spec.Executor.Annotations[prometheusPathAnnotation] = "/metrics"

		if app.Spec.Executor.JavaOptions == nil {
			app.Spec.Executor.JavaOptions = &javaOption
		} else {
			*app.Spec.Executor.JavaOptions = *app.Spec.Executor.JavaOptions + " " + javaOption
		}
	}

	return nil
}

func buildPrometheusConfigMap(app *v1beta1.SparkApplication, prometheusConfigMapName string) *corev1.ConfigMap {
	metricsProperties := config.DefaultMetricsProperties
	if app.Spec.Monitoring.MetricsProperties != nil {
		metricsProperties = *app.Spec.Monitoring.MetricsProperties
	}
	prometheusConfig := config.DefaultPrometheusConfiguration
	if app.Spec.Monitoring.Prometheus.Configuration != nil {
		prometheusConfig = *app.Spec.Monitoring.Prometheus.Configuration
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            prometheusConfigMapName,
			Namespace:       app.Namespace,
			OwnerReferences: []metav1.OwnerReference{*getOwnerReference(app)},
		},
		Data: map[string]string{
			metricsPropertiesKey: metricsProperties,
			prometheusConfigKey:  prometheusConfig,
		},
	}
}
