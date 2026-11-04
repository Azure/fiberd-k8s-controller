package podtemplate

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestBuildMetadata(t *testing.T) {
	cases := []struct {
		name            string
		labels          map[string]string
		annotations     map[string]string
		wantLabels      map[string]string
		wantAnnotations map[string]string // the hash annotation is checked apart
	}{
		{
			name:   "fiberd labels join the tenant's",
			labels: map[string]string{"app": "web", "tier": "edge"},
			wantLabels: map[string]string{
				"app":                   "web",
				"tier":                  "edge",
				v1alpha1.LabelGrant:     "web",
				v1alpha1.LabelGrantUID:  testUID,
				v1alpha1.LabelManagedBy: v1alpha1.ManagedBy,
			},
			wantAnnotations: map[string]string{},
		},
		{
			name: "fiberd labels override only their three keys",
			labels: map[string]string{
				"app":                   "web",
				v1alpha1.LabelGrant:     "other",
				v1alpha1.LabelGrantUID:  "forged",
				v1alpha1.LabelManagedBy: "helm",
				"fiberd.io/extra":       "kept",
			},
			wantLabels: map[string]string{
				"app":                   "web",
				"fiberd.io/extra":       "kept",
				v1alpha1.LabelGrant:     "web",
				v1alpha1.LabelGrantUID:  testUID,
				v1alpha1.LabelManagedBy: v1alpha1.ManagedBy,
			},
			wantAnnotations: map[string]string{},
		},
		{
			name: "no tenant labels",
			wantLabels: map[string]string{
				v1alpha1.LabelGrant:     "web",
				v1alpha1.LabelGrantUID:  testUID,
				v1alpha1.LabelManagedBy: v1alpha1.ManagedBy,
			},
			wantAnnotations: map[string]string{},
		},
		{
			name:            "tenant annotations stay and a forged hash is replaced",
			annotations:     map[string]string{"team": "a", AnnotationGrantHash: "forged"},
			wantLabels:      map[string]string{v1alpha1.LabelGrant: "web", v1alpha1.LabelGrantUID: testUID, v1alpha1.LabelManagedBy: v1alpha1.ManagedBy},
			wantAnnotations: map[string]string{"team": "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Labels = tc.labels
				cg.Spec.PodTemplate.Annotations = tc.annotations
			})
			tmpl := mustBuild(t, cg, testOptions())

			if diff := cmp.Diff(tc.wantLabels, tmpl.Labels); diff != "" {
				t.Errorf("labels mismatch (-want +got):\n%s", diff)
			}
			got := maps.Clone(tmpl.Annotations)
			h := got[AnnotationGrantHash]
			delete(got, AnnotationGrantHash)
			if diff := cmp.Diff(tc.wantAnnotations, got); diff != "" {
				t.Errorf("annotations mismatch (-want +got):\n%s", diff)
			}
			if want := mustHash(t, cg, testImage); h != want || h == "" {
				t.Errorf("%s = %q, want %q", AnnotationGrantHash, h, want)
			}
		})
	}
}

func TestBuildPodSpec(t *testing.T) {
	zygoteGate := corev1.PodReadinessGate{ConditionType: v1alpha1.PodConditionZygoteReady}
	tenantGate := corev1.PodReadinessGate{ConditionType: "example.com/warm"}
	cases := []struct {
		name               string
		mod                func(*corev1.PodSpec)
		serviceAccount     string
		wantServiceAccount string
		wantGates          []corev1.PodReadinessGate
	}{
		{
			name:               "defaults",
			wantServiceAccount: DefaultServiceAccount,
			wantGates:          []corev1.PodReadinessGate{zygoteGate},
		},
		{
			name:               "custom service account",
			serviceAccount:     "grant-runner",
			wantServiceAccount: "grant-runner",
			wantGates:          []corev1.PodReadinessGate{zygoteGate},
		},
		{
			name:               "a tenant cannot turn the token mount off",
			mod:                func(s *corev1.PodSpec) { s.AutomountServiceAccountToken = ptr.To(false) },
			wantServiceAccount: DefaultServiceAccount,
			wantGates:          []corev1.PodReadinessGate{zygoteGate},
		},
		{
			name:               "zygote gate is appended to the tenant's",
			mod:                func(s *corev1.PodSpec) { s.ReadinessGates = []corev1.PodReadinessGate{tenantGate} },
			wantServiceAccount: DefaultServiceAccount,
			wantGates:          []corev1.PodReadinessGate{tenantGate, zygoteGate},
		},
		{
			name:               "zygote gate is not repeated",
			mod:                func(s *corev1.PodSpec) { s.ReadinessGates = []corev1.PodReadinessGate{zygoteGate, tenantGate} },
			wantServiceAccount: DefaultServiceAccount,
			wantGates:          []corev1.PodReadinessGate{zygoteGate, tenantGate},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				if tc.mod != nil {
					tc.mod(&cg.Spec.PodTemplate.Spec)
				}
			})
			opts := testOptions()
			opts.ServiceAccount = tc.serviceAccount
			spec := mustBuild(t, cg, opts).Spec

			if spec.ServiceAccountName != tc.wantServiceAccount {
				t.Errorf("serviceAccountName = %q, want %q", spec.ServiceAccountName, tc.wantServiceAccount)
			}
			if spec.AutomountServiceAccountToken == nil || !*spec.AutomountServiceAccountToken {
				t.Errorf("automountServiceAccountToken = %v, want true", spec.AutomountServiceAccountToken)
			}
			if spec.RestartPolicy != corev1.RestartPolicyAlways {
				t.Errorf("restartPolicy = %q, want Always", spec.RestartPolicy)
			}
			if diff := cmp.Diff(tc.wantGates, spec.ReadinessGates); diff != "" {
				t.Errorf("readinessGates mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBuildPassThrough checks that the tenant fields the overlay does not
// own reach the Pod as written.
func TestBuildPassThrough(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*corev1.PodSpec)
		get  func(*corev1.PodSpec) any
	}{
		{
			name: "nodeSelector",
			mod:  func(s *corev1.PodSpec) { s.NodeSelector = map[string]string{"pool": "fibers"} },
			get:  func(s *corev1.PodSpec) any { return s.NodeSelector },
		},
		{
			name: "affinity",
			mod: func(s *corev1.PodSpec) {
				s.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
					}}},
				}}
			},
			get: func(s *corev1.PodSpec) any { return s.Affinity },
		},
		{
			name: "tolerations",
			mod: func(s *corev1.PodSpec) {
				s.Tolerations = []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
			},
			get: func(s *corev1.PodSpec) any { return s.Tolerations },
		},
		{
			name: "topologySpreadConstraints",
			mod: func(s *corev1.PodSpec) {
				s.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
					MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: corev1.DoNotSchedule,
				}}
			},
			get: func(s *corev1.PodSpec) any { return s.TopologySpreadConstraints },
		},
		{
			name: "imagePullSecrets",
			mod:  func(s *corev1.PodSpec) { s.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry"}} },
			get:  func(s *corev1.PodSpec) any { return s.ImagePullSecrets },
		},
		{
			name: "priorityClassName",
			mod:  func(s *corev1.PodSpec) { s.PriorityClassName = "fibers-high" },
			get:  func(s *corev1.PodSpec) any { return s.PriorityClassName },
		},
		{
			name: "terminationGracePeriodSeconds",
			mod:  func(s *corev1.PodSpec) { s.TerminationGracePeriodSeconds = ptr.To(int64(90)) },
			get:  func(s *corev1.PodSpec) any { return s.TerminationGracePeriodSeconds },
		},
		{
			name: "resourceClaims",
			mod: func(s *corev1.PodSpec) {
				s.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: ptr.To("one-gpu")}}
			},
			get: func(s *corev1.PodSpec) any { return s.ResourceClaims },
		},
		{
			name: "container workingDir",
			mod:  func(s *corev1.PodSpec) { s.Containers[0].WorkingDir = "/srv" },
			get:  func(s *corev1.PodSpec) any { return s.Containers[0].WorkingDir },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { tc.mod(&cg.Spec.PodTemplate.Spec) })
			want := tc.get(cg.Spec.PodTemplate.Spec.DeepCopy())
			spec := mustBuild(t, cg, testOptions()).Spec
			if diff := cmp.Diff(want, tc.get(&spec)); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", tc.name, diff)
			}
		})
	}
}

// TestBuildLeavesGrantAlone checks that Build works on a copy, and that two
// builds of one grant agree. The grant is the informer's cached copy, so
// the check on it is reflect.DeepEqual, which also sees the string a
// Quantity caches. cmp.Diff and equality.Semantic compare a Quantity by
// its value and would miss that. 512Mi is not in canonical form, so
// ParseQuantity leaves its string uncached.
func TestBuildLeavesGrantAlone(t *testing.T) {
	cases := []struct {
		name        string
		runtime     v1alpha1.Runtime
		writeBudget string
	}{
		{name: "proc", runtime: v1alpha1.RuntimeProc},
		{name: "gvisor", runtime: v1alpha1.RuntimeGvisor},
		{name: "proc with writeBudget", runtime: v1alpha1.RuntimeProc, writeBudget: "512Mi"},
		{name: "runc with writeBudget", runtime: v1alpha1.RuntimeRunc, writeBudget: "1536Mi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Runtime = tc.runtime
				container(cg).Args = []string{"-registry=example.com/zygotes"}
				container(cg).SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0))}
				if tc.writeBudget != "" {
					cg.Spec.WriteBudget = ptr.To(resource.MustParse(tc.writeBudget))
				}
			})
			before := cg.DeepCopy()
			first := mustBuild(t, cg, testOptions())
			second := mustBuild(t, cg, testOptions())
			if !reflect.DeepEqual(cg, before) {
				t.Errorf("Build changed the grant\n got %#v\nwant %#v", cg, before)
			}
			if diff := cmp.Diff(first, second); diff != "" {
				t.Errorf("two builds of one grant differ (-first +second):\n%s", diff)
			}
		})
	}
}

func TestBuildVolumes(t *testing.T) {
	tenantVolume := corev1.Volume{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	stateVol := corev1.Volume{Name: "fiberd-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	grantsVol := corev1.Volume{Name: "fiberd-grants", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
		Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("1Mi")),
	}}}
	stateMount := corev1.VolumeMount{Name: "fiberd-state", MountPath: "/var/lib/fiberd"}
	grantsMount := corev1.VolumeMount{Name: "fiberd-grants", MountPath: "/var/run/fiberd/grants"}
	cases := []struct {
		name        string
		volumes     []corev1.Volume
		mounts      []corev1.VolumeMount
		wantVolumes []corev1.Volume
		wantMounts  []corev1.VolumeMount
	}{
		{
			name:        "no tenant volumes",
			wantVolumes: []corev1.Volume{stateVol, grantsVol},
			wantMounts:  []corev1.VolumeMount{stateMount, grantsMount},
		},
		{
			name:        "tenant volumes come first",
			volumes:     []corev1.Volume{tenantVolume},
			mounts:      []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
			wantVolumes: []corev1.Volume{tenantVolume, stateVol, grantsVol},
			wantMounts:  []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}, stateMount, grantsMount},
		},
		{
			name:        "a mount above fiberd's directories is allowed",
			volumes:     []corev1.Volume{tenantVolume},
			mounts:      []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib"}},
			wantVolumes: []corev1.Volume{tenantVolume, stateVol, grantsVol},
			wantMounts:  []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib"}, stateMount, grantsMount},
		},
		{
			name:        "a sibling path with the same prefix is allowed",
			volumes:     []corev1.Volume{tenantVolume},
			mounts:      []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib/fiberd-cache"}},
			wantVolumes: []corev1.Volume{tenantVolume, stateVol, grantsVol},
			wantMounts:  []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib/fiberd-cache"}, stateMount, grantsMount},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Spec.Volumes = tc.volumes
				container(cg).VolumeMounts = tc.mounts
			})
			spec := mustBuild(t, cg, testOptions()).Spec
			if diff := cmp.Diff(tc.wantVolumes, spec.Volumes); diff != "" {
				t.Errorf("volumes mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantMounts, spec.Containers[0].VolumeMounts); diff != "" {
				t.Errorf("volumeMounts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildEnv(t *testing.T) {
	fieldEnv := func(name, path string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: path},
		}}
	}
	podEnv := []corev1.EnvVar{
		fieldEnv("POD_NAME", "metadata.name"),
		fieldEnv("POD_NAMESPACE", "metadata.namespace"),
		fieldEnv("POD_UID", "metadata.uid"),
	}
	// A ConfigMap key NAME under the prefix POD_ would reach the container
	// as POD_NAME. The kubelet lets an env entry win over envFrom, so the
	// explicit downward API entry must be there for -node-id to expand.
	identityFrom := []corev1.EnvFromSource{{
		Prefix:       "POD_",
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "identity"}},
	}}
	cases := []struct {
		name    string
		env     []corev1.EnvVar
		envFrom []corev1.EnvFromSource
		want    []corev1.EnvVar
	}{
		{name: "no tenant env", want: podEnv},
		{
			name: "tenant env comes first",
			env:  []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}},
			want: append([]corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}, podEnv...),
		},
		{
			name:    "envFrom cannot shadow the Pod identity",
			envFrom: identityFrom,
			want:    podEnv,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				container(cg).Env = tc.env
				container(cg).EnvFrom = tc.envFrom
			})
			tmpl := mustBuild(t, cg, testOptions())
			if diff := cmp.Diff(tc.want, tmpl.Spec.Containers[0].Env); diff != "" {
				t.Errorf("env mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.envFrom, tmpl.Spec.Containers[0].EnvFrom); diff != "" {
				t.Errorf("envFrom differs from what the tenant wrote (-want +got):\n%s", diff)
			}
			if got := tmpl.Spec.Containers[0].Image; got != testImage {
				t.Errorf("image = %q, want %q", got, testImage)
			}
		})
	}
}

func TestBuildPorts(t *testing.T) {
	grpc := corev1.ContainerPort{Name: "grpc", ContainerPort: 8484, Protocol: corev1.ProtocolTCP}
	metrics := corev1.ContainerPort{Name: "metrics", ContainerPort: 9090}
	cases := []struct {
		name  string
		ports []corev1.ContainerPort
		want  []corev1.ContainerPort
	}{
		{name: "no tenant ports", want: []corev1.ContainerPort{grpc}},
		{name: "tenant ports come first", ports: []corev1.ContainerPort{metrics}, want: []corev1.ContainerPort{metrics, grpc}},
		{
			name:  "8484 over UDP does not collide",
			ports: []corev1.ContainerPort{{Name: "dns", ContainerPort: 8484, Protocol: corev1.ProtocolUDP}},
			want:  []corev1.ContainerPort{{Name: "dns", ContainerPort: 8484, Protocol: corev1.ProtocolUDP}, grpc},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { container(cg).Ports = tc.ports })
			got := mustBuild(t, cg, testOptions()).Spec.Containers[0].Ports
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ports mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildErrors(t *testing.T) {
	type grantMod = func(*v1alpha1.CapacityGrant)
	podMod := func(f func(*corev1.PodSpec)) grantMod {
		return func(cg *v1alpha1.CapacityGrant) { f(&cg.Spec.PodTemplate.Spec) }
	}
	ctrMod := func(f func(*corev1.Container)) grantMod {
		return func(cg *v1alpha1.CapacityGrant) { f(container(cg)) }
	}
	emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	tenantProbe := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9090)}},
		PeriodSeconds: 7,
	}
	cases := []struct {
		name    string
		nilCG   bool
		noImage bool
		mod     grantMod
		opts    func(*Options)
		wantErr string
	}{
		{name: "nil grant", nilCG: true, wantErr: "nil CapacityGrant"},
		{name: "empty image", noImage: true, wantErr: "no resolved image"},
		// TestOptionsValidate covers each option. This case shows that Build
		// returns what Validate finds.
		{name: "invalid options", opts: func(o *Options) { o.IssuerURL = "" }, wantErr: "issuer URL is required"},
		{
			name:    "unknown runtime",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "kata" },
			wantErr: "spec.runtime",
		},
		{
			name:    "bad digest",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Digest = "sha256:ABC" },
			wantErr: "spec.zygote: Invalid value",
		},
		{
			name:    "empty zygote command",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Command = nil },
			wantErr: "spec.zygote: Invalid value",
		},
		{
			name:    "zygote command with a space",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Command = []string{"/app/zygote", "a b"} },
			wantErr: "spec.zygote: Invalid value",
		},
		{
			name: "two containers",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Containers = append(s.Containers, corev1.Container{Name: "sidecar"})
			}),
			wantErr: "spec.podTemplate.spec.containers",
		},
		{
			name:    "container with another name",
			mod:     ctrMod(func(c *corev1.Container) { c.Name = "app" }),
			wantErr: "exactly one container named fiberd",
		},
		{
			name:    "command set",
			mod:     ctrMod(func(c *corev1.Container) { c.Command = []string{"/bin/sh"} }),
			wantErr: "spec.podTemplate.spec.containers[0].command",
		},
		{
			name:    "init containers",
			mod:     podMod(func(s *corev1.PodSpec) { s.InitContainers = []corev1.Container{{Name: "init"}} }),
			wantErr: "spec.podTemplate.spec.initContainers",
		},
		{
			name:    "ephemeral containers",
			mod:     podMod(func(s *corev1.PodSpec) { s.EphemeralContainers = []corev1.EphemeralContainer{{}} }),
			wantErr: "spec.podTemplate.spec.ephemeralContainers",
		},
		{
			name:    "service account name",
			mod:     podMod(func(s *corev1.PodSpec) { s.ServiceAccountName = "admin" }),
			wantErr: "spec.podTemplate.spec.serviceAccountName",
		},
		{
			name:    "deprecated service account",
			mod:     podMod(func(s *corev1.PodSpec) { s.DeprecatedServiceAccount = "admin" }),
			wantErr: "spec.podTemplate.spec.serviceAccount",
		},
		{
			name:    "node name",
			mod:     podMod(func(s *corev1.PodSpec) { s.NodeName = "node-1" }),
			wantErr: "spec.podTemplate.spec.nodeName",
		},
		{
			name:    "restart policy Never",
			mod:     podMod(func(s *corev1.PodSpec) { s.RestartPolicy = corev1.RestartPolicyNever }),
			wantErr: "spec.podTemplate.spec.restartPolicy",
		},
		{
			name:    "host network",
			mod:     podMod(func(s *corev1.PodSpec) { s.HostNetwork = true }),
			wantErr: "spec.podTemplate.spec.hostNetwork",
		},
		{
			name:    "host PID",
			mod:     podMod(func(s *corev1.PodSpec) { s.HostPID = true }),
			wantErr: "spec.podTemplate.spec.hostPID",
		},
		{
			name:    "host IPC",
			mod:     podMod(func(s *corev1.PodSpec) { s.HostIPC = true }),
			wantErr: "spec.podTemplate.spec.hostIPC",
		},
		{
			name:    "shared process namespace",
			mod:     podMod(func(s *corev1.PodSpec) { s.ShareProcessNamespace = ptr.To(true) }),
			wantErr: "spec.podTemplate.spec.shareProcessNamespace",
		},
		{
			name:    "host users",
			mod:     podMod(func(s *corev1.PodSpec) { s.HostUsers = ptr.To(false) }),
			wantErr: "spec.podTemplate.spec.hostUsers",
		},
		{
			name:    "volume named fiberd-state",
			mod:     podMod(func(s *corev1.PodSpec) { s.Volumes = []corev1.Volume{{Name: "fiberd-state", VolumeSource: emptyDir}} }),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name:    "volume named fiberd-grants",
			mod:     podMod(func(s *corev1.PodSpec) { s.Volumes = []corev1.Volume{{Name: "fiberd-grants", VolumeSource: emptyDir}} }),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "mount of the grants volume elsewhere",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "fiberd-grants", MountPath: "/leak"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeMounts[0].name",
		},
		{
			name: "mount at the state path",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib/fiberd"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeMounts[0].mountPath",
		},
		{
			name: "mount at the state path with a trailing slash",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/var/lib/fiberd/"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeMounts[0].mountPath",
		},
		{
			name: "mount under the grants path",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/var/run/fiberd/grants/x"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeMounts[0].mountPath",
		},
		{
			name: "device under the state path",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeDevices = []corev1.VolumeDevice{{Name: "disk", DevicePath: "/var/lib/fiberd/dev"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeDevices[0].devicePath",
		},
		{
			name:    "env POD_NAME",
			mod:     ctrMod(func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "POD_NAME", Value: "forged"}} }),
			wantErr: "spec.podTemplate.spec.containers[0].env[0].name",
		},
		{
			name:    "port named grpc",
			mod:     ctrMod(func(c *corev1.Container) { c.Ports = []corev1.ContainerPort{{Name: "grpc", ContainerPort: 9000}} }),
			wantErr: "spec.podTemplate.spec.containers[0].ports[0].name",
		},
		{
			name:    "port 8484",
			mod:     ctrMod(func(c *corev1.Container) { c.Ports = []corev1.ContainerPort{{Name: "api", ContainerPort: 8484}} }),
			wantErr: "spec.podTemplate.spec.containers[0].ports[0].containerPort",
		},
		{
			name: "owned arg",
			mod: ctrMod(func(c *corev1.Container) {
				c.Args = []string{"-registry=example.com/z", "-issuer=https://evil.example"}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].args[1]",
		},
		{
			name:    "tenant template for the grant's digest",
			mod:     ctrMod(func(c *corev1.Container) { c.Args = []string{"-template=" + testDigest + "=/bin/sh"} }),
			wantErr: "spec.podTemplate.spec.containers[0].args[0]: Forbidden: -template is set by the controller",
		},
		{
			name:    "tenant pod-name flag",
			mod:     ctrMod(func(c *corev1.Container) { c.Args = []string{"-pod-name=forged"} }),
			wantErr: "spec.podTemplate.spec.containers[0].args[0]: Forbidden: -pod-name is set by the controller",
		},
		{
			name:    "tenant startup probe",
			mod:     ctrMod(func(c *corev1.Container) { c.StartupProbe = tenantProbe }),
			wantErr: "spec.podTemplate.spec.containers[0].startupProbe: Forbidden: the controller sets fiberd's probes",
		},
		{
			name:    "tenant readiness probe",
			mod:     ctrMod(func(c *corev1.Container) { c.ReadinessProbe = tenantProbe }),
			wantErr: "spec.podTemplate.spec.containers[0].readinessProbe: Forbidden: the controller sets fiberd's probes",
		},
		{
			name:    "tenant liveness probe",
			mod:     ctrMod(func(c *corev1.Container) { c.LivenessProbe = tenantProbe }),
			wantErr: "spec.podTemplate.spec.containers[0].livenessProbe: Forbidden: the controller sets fiberd's probes",
		},
		{
			name: "pod-level resources",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}
			}),
			wantErr: "spec.podTemplate.spec.resources: Forbidden",
		},
		{
			name:    "missing memory limit",
			mod:     ctrMod(func(c *corev1.Container) { c.Resources = corev1.ResourceRequirements{} }),
			wantErr: "spec.podTemplate.spec.containers[0].resources.limits[memory]",
		},
		{
			name: "memory only in requests",
			mod: ctrMod(func(c *corev1.Container) {
				c.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].resources.limits[memory]",
		},
		{
			name: "zero memory limit",
			mod: ctrMod(func(c *corev1.Container) {
				c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("0")}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].resources.limits[memory]",
		},
		{
			name: "zero CPU limit",
			mod: ctrMod(func(c *corev1.Container) {
				c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("0")
			}),
			wantErr: "spec.podTemplate.spec.containers[0].resources.limits[cpu]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cg *v1alpha1.CapacityGrant
			if !tc.nilCG {
				cg = testGrant()
				if tc.mod != nil {
					tc.mod(cg)
				}
			}
			image := testImage
			if tc.noImage {
				image = ""
			}
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			_, err := Build(cg, image, opts)
			if err == nil {
				t.Fatalf("Build succeeded, want an error holding %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Build error = %q, want it to hold %q", err, tc.wantErr)
			}
		})
	}
}
