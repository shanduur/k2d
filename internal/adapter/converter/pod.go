package converter

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	k2dtypes "github.com/portainer/k2d/internal/adapter/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/core"
)

// ConvertContainerToPod tries to convert a Docker container into a Kubernetes Pod.
// It only implements partial conversion at the moment.
func (converter *DockerAPIConverter) ConvertContainerToPod(container types.Container) core.Pod {
	containerName := strings.TrimPrefix(container.Names[0], "/")
	containerState := container.State

	pod := core.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              containerName,
			CreationTimestamp: metav1.NewTime(time.Unix(container.Created, 0)),
			Namespace:         "default",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": container.Labels[k2dtypes.WorkloadLastAppliedConfigLabelKey],
			},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name:  containerName,
					Image: container.Image,
				},
			},
		},
		Status: core.PodStatus{
			ContainerStatuses: []core.ContainerStatus{
				{
					Name:         containerName,
					ContainerID:  container.ID,
					RestartCount: 0,
				},
			},
		},
	}

	if containerState == "running" {
		ready := true

		pod.Status.Phase = core.PodRunning

		pod.Status.ContainerStatuses[0].Ready = ready
		pod.Status.ContainerStatuses[0].Started = &ready

		pod.Status.ContainerStatuses[0].State.Running = &core.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Unix(container.Created, 0)),
		}
	} else {
		pod.Status.Phase = core.PodUnknown
	}

	return pod
}

// ConvertPodSpecToContainerConfiguration converts a Kubernetes PodSpec into a Docker container configuration.
// It receives a Kubernetes PodSpec and a map of labels.
// It returns a ContainerConfiguration struct, or an error if the conversion fails.
func (converter *DockerAPIConverter) ConvertPodSpecToContainerConfiguration(spec core.PodSpec, labels map[string]string) (ContainerConfiguration, error) {
	containerSpec := spec.Containers[0]

	containerConfig := &container.Config{
		Image:  containerSpec.Image,
		Labels: labels,
		Env: []string{
			fmt.Sprintf("KUBERNETES_SERVICE_HOST=%s", converter.k2dServerConfiguration.ServerIpAddr),
			fmt.Sprintf("KUBERNETES_SERVICE_PORT=%d", converter.k2dServerConfiguration.ServerPort),
		},
	}

	hostConfig := &container.HostConfig{
		ExtraHosts: []string{
			fmt.Sprintf("kubernetes.default.svc:%s", converter.k2dServerConfiguration.ServerIpAddr),
		},
	}

	if err := converter.setServiceAccountTokenAndCACert(hostConfig); err != nil {
		return ContainerConfiguration{}, err
	}

	if err := converter.setHostPorts(containerConfig, hostConfig, containerSpec.Ports); err != nil {
		return ContainerConfiguration{}, err
	}

	if err := converter.setEnvVars(containerConfig, containerSpec.Env, containerSpec.EnvFrom); err != nil {
		return ContainerConfiguration{}, err
	}

	setCommandAndArgs(containerConfig, containerSpec.Command, containerSpec.Args)
	setRestartPolicy(hostConfig, spec.RestartPolicy)
	setSecurityContext(containerConfig, hostConfig, spec.SecurityContext, containerSpec.SecurityContext)
	converter.setResourceRequirements(hostConfig, containerSpec.Resources)

	if err := converter.setVolumeMounts(hostConfig, spec.Volumes, containerSpec.VolumeMounts); err != nil {
		return ContainerConfiguration{}, err
	}

	return ContainerConfiguration{
		ContainerConfig: containerConfig,
		HostConfig:      hostConfig,
		NetworkConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				k2dtypes.K2DNetworkName: {},
			},
		},
	}, nil
}

// setResourceRequirements configures the Docker container's resource constraints based on the provided core.ResourceRequirements.
// It receives a Docker HostConfig and a Kubernetes ResourceRequirements.
// It returns nothing.
func (converter *DockerAPIConverter) setResourceRequirements(hostConfig *container.HostConfig, resources core.ResourceRequirements) {
	resourceRequirements := container.Resources{}
	if resources.Requests != nil {
		for resourceName, quantity := range resources.Requests {
			switch resourceName {
			case core.ResourceCPU:
				resourceRequirements.CPUShares = int64(quantity.MilliValue())
			case core.ResourceMemory:
				resourceRequirements.MemoryReservation = int64(quantity.Value())
			}
		}
	}

	if resources.Limits != nil {
		for resourceName, quantity := range resources.Limits {
			switch resourceName {
			case core.ResourceCPU:
				resourceRequirements.NanoCPUs = int64(quantity.MilliValue()) * 1000000
			case core.ResourceMemory:
				resourceRequirements.Memory = int64(quantity.Value())
			}
		}
	}

	hostConfig.Resources = resourceRequirements
}

// setServiceAccountTokenAndCACert configures the Docker container to have access to the service account token
// and CA certificate stored in a Kubernetes Secret. The function performs the following steps:
//  1. Fetches the service account Secret from Kubernetes using the provided secretStore.
//  2. Obtains the filesystem bind mappings for the Secret using the secretStore's GetSecretBinds method.
//  3. Modifies the hostConfig's Binds field to include the service account token and CA certificate by
//     mapping the host file system paths to the container's "/var/run/secrets/kubernetes.io/serviceaccount/" directory.
//
// Parameters:
//   - hostConfig: The Docker container's host configuration that will be modified to include the service
//     account token and CA certificate binds.
//
// It returns an error if any occurred fetching the Secret or obtaining the bind mappings fails.
func (converter *DockerAPIConverter) setServiceAccountTokenAndCACert(hostConfig *container.HostConfig) error {
	secret, err := converter.secretStore.GetSecret(k2dtypes.K2dServiceAccountSecretName)
	if err != nil {
		return fmt.Errorf("unable to get secret %s: %w", k2dtypes.K2dServiceAccountSecretName, err)
	}

	binds, err := converter.secretStore.GetSecretBinds(secret)
	if err != nil {
		return fmt.Errorf("unable to get binds for secrets %s: %w", k2dtypes.K2dServiceAccountSecretName, err)
	}

	for containerBind, hostBind := range binds {
		bind := fmt.Sprintf("%s:%s", hostBind, path.Join("/var/run/secrets/kubernetes.io/serviceaccount/", containerBind))
		hostConfig.Binds = append(hostConfig.Binds, bind)
	}

	return nil
}

// setHostPorts configures the Docker container's ports based on the provided core.ContainerPort slices (coming from the pod specs).
// It iterates through the ports and sets both the container's exposed ports (inside the container) and
// the host's port bindings (on the host machine). Ports are mapped only if the HostPort is not zero.
// The mappings are applied to the provided containerConfig and hostConfig.
// It returns an error if any occurred during the port conversion or mapping process.
func (converter *DockerAPIConverter) setHostPorts(containerConfig *container.Config, hostConfig *container.HostConfig, ports []core.ContainerPort) error {
	containerPortMaps := nat.PortMap{}
	containerExposedPorts := nat.PortSet{}

	for _, port := range ports {
		if port.HostPort != 0 {
			containerPort, err := nat.NewPort(string(port.Protocol), strconv.Itoa(int(port.ContainerPort)))
			if err != nil {
				return err
			}

			hostBinding := nat.PortBinding{
				HostIP:   "0.0.0.0",
				HostPort: strconv.Itoa(int(port.HostPort)),
			}

			containerPortMaps[containerPort] = []nat.PortBinding{hostBinding}
			containerExposedPorts[containerPort] = struct{}{}
		}
	}

	containerConfig.ExposedPorts = containerExposedPorts
	hostConfig.PortBindings = containerPortMaps

	return nil
}

// setEnvVars handles setting the environment variables for the Docker container configuration.
// It receives a pointer to the container configuration and an array of Kubernetes environment variables.
// It returns an error if the setting of environment variables fails.
func (converter *DockerAPIConverter) setEnvVars(containerConfig *container.Config, envs []core.EnvVar, envFrom []core.EnvFromSource) error {
	for _, env := range envs {

		if env.ValueFrom != nil {
			if err := converter.handleValueFromEnvVars(containerConfig, env); err != nil {
				return err
			}
		} else {
			containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s=%s", env.Name, env.Value))
		}
	}

	for _, env := range envFrom {
		if err := converter.handleValueFromEnvFromSource(containerConfig, env); err != nil {
			return err
		}
	}

	return nil
}

// handleValueFromEnvFromSource populates the environment variables of a Docker container configuration from
// a Kubernetes EnvFromSource object. The function supports environment variables defined in ConfigMaps and Secrets.
//
// The function takes two parameters:
// - containerConfig: a pointer to a Docker container configuration object where the environment variables will be added.
// - env: a Kubernetes EnvFromSource object that contains the source of the environment variables.
//
// If the EnvFromSource object points to a ConfigMap, the function retrieves the ConfigMap and adds its data as
// environment variables to the Docker container configuration. Similarly, if the EnvFromSource points to a Secret,
// the function retrieves the Secret and adds its data as environment variables.
func (converter *DockerAPIConverter) handleValueFromEnvFromSource(containerConfig *container.Config, env core.EnvFromSource) error {
	if env.ConfigMapRef != nil {
		configMap, err := converter.configMapStore.GetConfigMap(env.ConfigMapRef.Name)
		if err != nil {
			return fmt.Errorf("unable to get configmap %s: %w", env.ConfigMapRef.Name, err)
		}

		for key, value := range configMap.Data {
			containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s=%s", key, value))
		}
	} else if env.SecretRef != nil {
		secret, err := converter.secretStore.GetSecret(env.SecretRef.Name)
		if err != nil {
			return fmt.Errorf("unable to get secret %s: %w", env.SecretRef.Name, err)
		}

		for key, value := range secret.Data {
			containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s=%s", key, value))
		}
	}

	return nil
}

// handleValueFromEnvVars manages environment variables that are defined through ConfigMap or Secret references.
// It receives a pointer to the container configuration and a Kubernetes environment variable.
// It returns an error if the sourcing of the environment variables fails.
func (converter *DockerAPIConverter) handleValueFromEnvVars(containerConfig *container.Config, env core.EnvVar) error {
	if env.ValueFrom.ConfigMapKeyRef != nil {
		configMap, err := converter.configMapStore.GetConfigMap(env.ValueFrom.ConfigMapKeyRef.Name)
		if err != nil {
			return fmt.Errorf("unable to get configmap %s: %w", env.ValueFrom.ConfigMapKeyRef.Name, err)
		}

		containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s=%s", env.Name, configMap.Data[env.ValueFrom.ConfigMapKeyRef.Key]))
	} else if env.ValueFrom.SecretKeyRef != nil {
		secret, err := converter.secretStore.GetSecret(env.ValueFrom.SecretKeyRef.Name)
		if err != nil {
			return fmt.Errorf("unable to get secret %s: %w", env.ValueFrom.SecretKeyRef.Name, err)
		}

		containerConfig.Env = append(containerConfig.Env, fmt.Sprintf("%s=%s", env.Name, secret.Data[env.ValueFrom.SecretKeyRef.Key]))
	}
	return nil
}

// setRestartPolicy sets the Docker container's restart policy according to the Kubernetes pod's restart policy.
// It receives a pointer to the host configuration and the Kubernetes pod's restart policy.
func setRestartPolicy(hostConfig *container.HostConfig, restartPolicy core.RestartPolicy) {
	switch restartPolicy {
	case "OnFailure":
		hostConfig.RestartPolicy = container.RestartPolicy{Name: "on-failure"}
	case "Never":
		hostConfig.RestartPolicy = container.RestartPolicy{Name: "no"}
	default:
		hostConfig.RestartPolicy = container.RestartPolicy{Name: "always"}
	}
}

// setCommandAndArgs configures the entrypoint and command arguments for a given Docker container configuration.
// If the 'command' slice is non-empty, it is set as the container's entrypoint.
// If the 'args' slice is non-empty, it is set as the container's command arguments.
func setCommandAndArgs(containerConfig *container.Config, command []string, args []string) {
	if len(command) > 0 {
		containerConfig.Entrypoint = command
	}

	if len(args) > 0 {
		containerConfig.Cmd = args
	}
}

// setSecurityContext sets the user and group ID in the Docker container configuration based on the provided
// Kubernetes PodSecurityContext.
// If no security context is provided, the function does not modify the container configuration.
func setSecurityContext(config *container.Config, hostConfig *container.HostConfig, podSecurityContext *core.PodSecurityContext, containerSecurityContext *core.SecurityContext) {
	if podSecurityContext == nil {
		return
	}

	if podSecurityContext.RunAsUser != nil && podSecurityContext.RunAsGroup != nil {
		config.User = fmt.Sprintf("%d:%d", *podSecurityContext.RunAsUser, *podSecurityContext.RunAsGroup)
	}

	if containerSecurityContext == nil {
		return
	}

	if containerSecurityContext.Privileged != nil {
		hostConfig.Privileged = *containerSecurityContext.Privileged
	}
}

// setVolumeMounts manages volume mounts for the Docker container.
// It receives a pointer to the host configuration, an array of Kubernetes volumes, and an array of Kubernetes volume mounts.
// It returns an error if the handling of volume mounts fails.
func (converter *DockerAPIConverter) setVolumeMounts(hostConfig *container.HostConfig, volumes []core.Volume, volumeMounts []core.VolumeMount) error {
	for _, volume := range volumes {
		for _, volumeMount := range volumeMounts {
			if volumeMount.Name == volume.Name {
				if err := converter.handleVolumeSource(hostConfig, volume, volumeMount); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// handleVolumeSource processes a Kubernetes VolumeSource, which can either be a ConfigMap, a Secret, or a HostPath.
// This function sets up the volume bindings in the Docker host configuration according to the provided VolumeSource.
//
// For ConfigMap and Secret:
// 1. It fetches the resource (ConfigMap or Secret) from the store.
// 2. It generates the list of filesystem binds via the store specific implementation.
// 3. These binds are then appended to the Docker host configuration.
//
// For HostPath:
// The function directly uses the HostPath and volume mount to set up the bind in the Docker host configuration.
//
// Parameters:
// - hostConfig:   The Docker host configuration where the volume binds will be set.
// - volume:       The Kubernetes volume object to be processed.
// - volumeMount:  The Kubernetes volume mount object that provides additional information on how the volume should be mounted.
//
// Returns:
// An error if fetching the ConfigMap or Secret from the store fails; otherwise, it returns nil.
func (converter *DockerAPIConverter) handleVolumeSource(hostConfig *container.HostConfig, volume core.Volume, volumeMount core.VolumeMount) error {
	if volume.VolumeSource.ConfigMap != nil {
		configMap, err := converter.configMapStore.GetConfigMap(volume.VolumeSource.ConfigMap.Name)
		if err != nil {
			return fmt.Errorf("unable to get configmap %s: %w", volume.VolumeSource.ConfigMap.Name, err)
		}

		binds, err := converter.configMapStore.GetConfigMapBinds(configMap)
		if err != nil {
			return fmt.Errorf("unable to get binds for configmap %s: %w", volume.VolumeSource.ConfigMap.Name, err)
		}

		for containerBind, hostBind := range binds {
			bind := fmt.Sprintf("%s:%s", hostBind, path.Join(volumeMount.MountPath, containerBind))
			hostConfig.Binds = append(hostConfig.Binds, bind)
		}
	} else if volume.VolumeSource.Secret != nil {
		secret, err := converter.secretStore.GetSecret(volume.VolumeSource.Secret.SecretName)
		if err != nil {
			return fmt.Errorf("unable to get secret %s: %w", volume.VolumeSource.Secret.SecretName, err)
		}

		binds, err := converter.secretStore.GetSecretBinds(secret)
		if err != nil {
			return fmt.Errorf("unable to get binds for secrets %s: %w", volume.VolumeSource.ConfigMap.Name, err)
		}

		for containerBind, hostBind := range binds {
			bind := fmt.Sprintf("%s:%s", hostBind, path.Join(volumeMount.MountPath, containerBind))
			hostConfig.Binds = append(hostConfig.Binds, bind)
		}
	} else if volume.HostPath != nil {
		bind := fmt.Sprintf("%s:%s", volume.HostPath.Path, volumeMount.MountPath)
		hostConfig.Binds = append(hostConfig.Binds, bind)
	}
	return nil
}
