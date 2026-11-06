package podtemplate

import (
	"maps"
	"reflect"
	"strconv"
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
			// TestBuildErrors covers the fiberd.io keys, which Build refuses.
			name:   "the managed-by label is the controller's",
			labels: map[string]string{"app": "web", v1alpha1.LabelManagedBy: "helm"},
			wantLabels: map[string]string{
				"app":                   "web",
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
			name:            "tenant annotations join the hash",
			annotations:     map[string]string{"team": "a"},
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
			name:               "a tenant cannot turn service links on",
			mod:                func(s *corev1.PodSpec) { s.EnableServiceLinks = ptr.To(true) },
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
			// Kubernetes reads an unset field as true.
			if got := ptr.Deref(spec.EnableServiceLinks, true); got {
				t.Errorf("enableServiceLinks = %t, want false", got)
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
				container(cg).Args = []string{"-delta-registry=example.com/deltas"}
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
	// The attributes are literal so a typo in a key or value fails here.
	tlsVol := corev1.Volume{Name: "fiberd-tls", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:   "csi.cert-manager.io",
		ReadOnly: ptr.To(true),
		VolumeAttributes: map[string]string{
			"csi.cert-manager.io/issuer-name":       "fiberd-ca",
			"csi.cert-manager.io/issuer-kind":       "ClusterIssuer",
			"csi.cert-manager.io/common-name":       "${POD_NAME}",
			"csi.cert-manager.io/dns-names":         "fiberd-web.team-a.svc,fiberd-web.team-a.svc.cluster.local",
			"csi.cert-manager.io/key-usages":        "server auth",
			"csi.cert-manager.io/duration":          "24h",
			"csi.cert-manager.io/renew-before":      "8h",
			"csi.cert-manager.io/reuse-private-key": "false",
		},
	}}}
	tokenVol := corev1.Volume{Name: "fiberd-grant-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience:          testIssuer,
			ExpirationSeconds: ptr.To[int64](600),
			Path:              "token",
		}}},
	}}}
	stateMount := corev1.VolumeMount{Name: "fiberd-state", MountPath: "/var/lib/fiberd"}
	grantsMount := corev1.VolumeMount{Name: "fiberd-grants", MountPath: "/var/run/fiberd/grants"}
	tlsMount := corev1.VolumeMount{Name: "fiberd-tls", MountPath: "/var/run/fiberd/tls", ReadOnly: true}
	tokenMount := corev1.VolumeMount{Name: "fiberd-grant-token", MountPath: "/var/run/secrets/fiberd.io/grant", ReadOnly: true}
	sealVol := corev1.Volume{Name: "fiberd-seal-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName:  "team-a-seal",
		Items:       []corev1.KeyToPath{{Key: "seal-key.json", Path: "seal-key.json"}},
		DefaultMode: ptr.To[int32](0o400),
	}}}
	sealMount := corev1.VolumeMount{Name: "fiberd-seal-key", MountPath: "/var/run/secrets/fiberd.io/seal-key", ReadOnly: true}
	cases := []struct {
		name        string
		zygote      *v1alpha1.ZygoteSpec
		volumes     []corev1.Volume
		mounts      []corev1.VolumeMount
		wantVolumes []corev1.Volume
		wantMounts  []corev1.VolumeMount
	}{
		{
			name:        "no tenant volumes",
			wantVolumes: []corev1.Volume{stateVol, grantsVol, tlsVol, tokenVol},
			wantMounts:  []corev1.VolumeMount{stateMount, grantsMount, tlsMount, tokenMount},
		},
		{
			name:        "tenant volumes come first",
			volumes:     []corev1.Volume{tenantVolume},
			mounts:      []corev1.VolumeMount{{Name: "cache", MountPath: "/data/cache"}},
			wantVolumes: []corev1.Volume{tenantVolume, stateVol, grantsVol, tlsVol, tokenVol},
			wantMounts:  []corev1.VolumeMount{{Name: "cache", MountPath: "/data/cache"}, stateMount, grantsMount, tlsMount, tokenMount},
		},
		{
			name:        "a registry zygote without a seal key adds no volume",
			zygote:      &v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/a/z"},
			wantVolumes: []corev1.Volume{stateVol, grantsVol, tlsVol, tokenVol},
			wantMounts:  []corev1.VolumeMount{stateMount, grantsMount, tlsMount, tokenMount},
		},
		{
			name:        "a seal key Secret is mounted read-only",
			zygote:      &v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/a/z", SealKeySecretName: "team-a-seal"},
			wantVolumes: []corev1.Volume{stateVol, grantsVol, tlsVol, tokenVol, sealVol},
			wantMounts:  []corev1.VolumeMount{stateMount, grantsMount, tlsMount, tokenMount, sealMount},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				if tc.zygote != nil {
					cg.Spec.Zygote = *tc.zygote
				}
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

// TestBuildMountPaths runs every path through each field that takes one, since
// they share one rule. The policy's envtest table repeats these paths. An
// empty path is the unset termination message path, so it has its own rows in
// TestBuildErrors.
func TestBuildMountPaths(t *testing.T) {
	const (
		unclean = "must be a clean absolute path"
		outside = "Forbidden: must be at or under /config, /data, /secrets"
	)
	fields := []struct {
		name string
		set  func(c *corev1.Container, p string)
	}{
		{
			name: "volumeMounts[0].mountPath",
			set: func(c *corev1.Container, p string) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: p}}
			},
		},
		{
			name: "volumeDevices[0].devicePath",
			set: func(c *corev1.Container, p string) {
				c.VolumeDevices = []corev1.VolumeDevice{{Name: "disk", DevicePath: p}}
			},
		},
		{
			name: "terminationMessagePath",
			set:  func(c *corev1.Container, p string) { c.TerminationMessagePath = p },
		},
	}
	cases := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "the data root", path: "/data"},
		{name: "the config root", path: "/config"},
		{name: "the secrets root", path: "/secrets"},
		{name: "under the data root", path: "/data/cache"},
		{name: "under the config root", path: "/config/app/settings"},
		{name: "under the secrets root", path: "/secrets/seal"},
		{name: "a name of three dots", path: "/data/..."},
		{name: "the fiberd binary", path: "/usr/local/bin/fiberd", wantErr: outside},
		{name: "the fiberd binary's directory", path: "/usr/local/bin", wantErr: outside},
		{name: "a system library directory", path: "/usr/lib", wantErr: outside},
		{name: "the loader preload file", path: "/etc/ld.so.preload", wantErr: outside},
		{name: "the system trust roots", path: "/etc/ssl/certs", wantErr: outside},
		{name: "the resolver file", path: "/etc/resolv.conf", wantErr: outside},
		{name: "the state directory", path: "/var/lib/fiberd", wantErr: outside},
		{name: "the grants directory", path: "/var/run/fiberd/grants", wantErr: outside},
		{name: "the grant token directory", path: "/var/run/secrets/fiberd.io/grant", wantErr: outside},
		{name: "the helper directory", path: "/var/run/fiberd/helper", wantErr: outside},
		{name: "the filesystem root", path: "/", wantErr: outside},
		{name: "a look-alike of the data root", path: "/database", wantErr: outside},
		{name: "a look-alike of the secrets root", path: "/secrets-old/key", wantErr: outside},
		{name: "another case of a root", path: "/Data", wantErr: outside},
		{name: "a trailing slash outside the roots", path: "/etc/", wantErr: unclean},
		{name: "a trailing slash on a root", path: "/data/", wantErr: unclean},
		{name: "a parent segment out of a root", path: "/data/../etc", wantErr: unclean},
		{name: "a parent segment that stays in a root", path: "/data/a/../b", wantErr: unclean},
		{name: "a final parent segment", path: "/data/..", wantErr: unclean},
		{name: "a dot segment", path: "/data/./cache", wantErr: unclean},
		{name: "a final dot segment", path: "/data/.", wantErr: unclean},
		{name: "an empty segment", path: "/data//cache", wantErr: unclean},
		{name: "a leading double slash", path: "//data", wantErr: unclean},
		{name: "a relative path", path: "data/cache", wantErr: unclean},
		{name: "a relative root", path: "data", wantErr: unclean},
		{name: "a leading dot", path: "./data", wantErr: unclean},
	}
	for _, f := range fields {
		for _, tc := range cases {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				cg := testGrant(func(cg *v1alpha1.CapacityGrant) { f.set(container(cg), tc.path) })
				_, err := Build(cg, testImage, testServiceName, testOptions())
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("Build: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("Build succeeded, want an error holding %q", tc.wantErr)
				}
				for _, want := range []string{f.name, tc.wantErr} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Build error = %q, want it to hold %q", err, want)
					}
				}
			})
		}
	}
}

// TestBuildOverlayAvoidsMountRoots ties the overlay to the tenant's rules.
// Every volume it adds carries a name a tenant may not use, and no mount of it
// is at, under or above a root, where a tenant mount could replace it or hide it.
// Each grant names a seal key, so the seal key's volume and mount are among them.
func TestBuildOverlayAvoidsMountRoots(t *testing.T) {
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
	}{
		{name: "proc", runtime: v1alpha1.RuntimeProc},
		{name: "runc", runtime: v1alpha1.RuntimeRunc},
		{name: "gvisor", runtime: v1alpha1.RuntimeGvisor},
		{name: "hyperlight", runtime: v1alpha1.RuntimeHyperlight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, tc.runtime)
				cg.Spec.Zygote.SealKeySecretName = "seal"
			})
			spec := mustBuild(t, cg, testOptions()).Spec
			for _, v := range spec.Volumes {
				if !isOwnedVolume(v.Name) {
					t.Errorf("overlay volume %s is not in ownedVolumes", v.Name)
				}
			}
			mounts := spec.Containers[0].VolumeMounts
			for _, c := range spec.InitContainers {
				mounts = append(mounts, c.VolumeMounts...)
			}
			for _, m := range mounts {
				if !isOwnedVolume(m.Name) {
					t.Errorf("overlay mount %s names a volume outside ownedVolumes", m.Name)
				}
				for _, r := range mountRoots {
					if m.MountPath == r || strings.HasPrefix(m.MountPath, r+"/") || strings.HasPrefix(r, m.MountPath+"/") {
						t.Errorf("overlay mount %s at %s collides with the mount root %s", m.Name, m.MountPath, r)
					}
				}
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
	overlayEnv := []corev1.EnvVar{
		fieldEnv("POD_NAME", "metadata.name"),
		fieldEnv("POD_NAMESPACE", "metadata.namespace"),
		fieldEnv("POD_UID", "metadata.uid"),
		{Name: "SSL_CERT_FILE", Value: "/var/run/fiberd/tls/ca.crt"},
	}
	fromSecret := corev1.EnvVar{Name: "FIBERD_TENANT_TOKEN", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "helper"}, Key: "token"},
	}}
	cases := []struct {
		name string
		env  []corev1.EnvVar
		want []corev1.EnvVar
	}{
		{name: "no tenant env", want: overlayEnv},
		{
			name: "tenant env comes first",
			env:  []corev1.EnvVar{{Name: "FIBERD_TENANT_LOG_LEVEL", Value: "debug"}},
			want: append([]corev1.EnvVar{{Name: "FIBERD_TENANT_LOG_LEVEL", Value: "debug"}}, overlayEnv...),
		},
		{
			name: "tenant env may take its value from a Secret",
			env:  []corev1.EnvVar{fromSecret},
			want: append([]corev1.EnvVar{fromSecret}, overlayEnv...),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { container(cg).Env = tc.env })
			tmpl := mustBuild(t, cg, testOptions())
			if diff := cmp.Diff(tc.want, tmpl.Spec.Containers[0].Env); diff != "" {
				t.Errorf("env mismatch (-want +got):\n%s", diff)
			}
			if got := tmpl.Spec.Containers[0].Image; got != testImage {
				t.Errorf("image = %q, want %q", got, testImage)
			}
		})
	}
}

// TestBuildEnvNames runs the name rule on a grant stored before the rule,
// which reaches the builder without passing the policy. The entry under the
// root comes first, so the error must name the second.
func TestBuildEnvNames(t *testing.T) {
	refused := []string{
		"",
		"PATH",
		"LD_AUDIT",
		"BASH_ENV",
		"GRPC_ENFORCE_ALPN_ENABLED",
		"EXAMPLE_CLIENT_ID",
		"fiberd_tenant_x",
		"FIBERD_TENANT_x",
		"FIBERD_TENANT_",
		"FIBERD_TENANT_X\nPATH",
		"FIBERD_TENANT_X\n",
		"MY_FIBERD_TENANT_X",
	}
	for _, name := range refused {
		t.Run(name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				container(cg).Env = []corev1.EnvVar{{Name: "FIBERD_TENANT_X", Value: "x"}, {Name: name, Value: "x"}}
			})
			_, err := Build(cg, testImage, testServiceName, testOptions())
			if err == nil {
				t.Fatal("Build succeeded, want the name refused")
			}
			want := "env[1].name: Forbidden: " + strconv.Quote(name) + " must match " + EnvNamePattern
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Build error = %q, want it to hold %q", err, want)
			}
		})
	}
}

// TestBuildOverlayEnvOutsideRoot keeps the overlay's names apart from the
// tenant's, so no tenant name can shadow one.
func TestBuildOverlayEnvOutsideRoot(t *testing.T) {
	for _, e := range mustBuild(t, testGrant(), testOptions()).Spec.Containers[0].Env {
		if envNamePattern.MatchString(e.Name) {
			t.Errorf("overlay variable %s matches %s", e.Name, EnvNamePattern)
		}
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
	// Two rows share it, so each proves one of the two errors is reported.
	commandAndHook := ctrMod(func(c *corev1.Container) {
		c.Command = []string{"/bin/sh"}
		c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh"}}}}
	})
	const envWhy = "must match " + EnvNamePattern
	const sourceWhy = "Forbidden: must be an emptyDir, configMap, secret, downwardAPI, projected, persistentVolumeClaim or ephemeral volume"
	tenantProbe := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9090)}},
		PeriodSeconds: 7,
	}
	cases := []struct {
		name      string
		nilCG     bool
		noImage   bool
		noService bool
		mod       grantMod
		opts      func(*Options)
		wantErr   string
	}{
		{name: "nil grant", nilCG: true, wantErr: "nil CapacityGrant"},
		{name: "empty image", noImage: true, wantErr: "no resolved image"},
		{name: "empty service name", noService: true, wantErr: "no Service name"},
		// TestOptionsValidate covers each option. This case shows that Build
		// returns what Validate finds.
		{name: "invalid options", opts: func(o *Options) { o.IssuerURL = "" }, wantErr: "issuer URL is required"},
		{
			name:    "unknown runtime",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "kata" },
			wantErr: "spec.runtime",
		},
		{
			name:    "hyperlight without a helper image",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeHyperlight },
			wantErr: "spec.helperImage: Required value",
		},
		{
			name: "a helper image on another runtime",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Runtime = v1alpha1.RuntimeGvisor
				cg.Spec.HelperImage = testHelperImage
			},
			wantErr: "spec.helperImage: Forbidden: allowed only for runtime hyperlight",
		},
		{
			name: "a helper image on the unset runtime",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Runtime = ""
				cg.Spec.HelperImage = testHelperImage
			},
			wantErr: "spec.helperImage: Forbidden: allowed only for runtime hyperlight",
		},
		{
			// The snapshot a hyperlight registry zygote pulls is sealed. The
			// same grant with a seal key builds in TestBuildArgs.
			name: "a hyperlight registry zygote without a seal key",
			mod: func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, v1alpha1.RuntimeHyperlight)
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/team-a/zygotes"}
			},
			wantErr: "spec.zygote.sealKeySecretName: Required value",
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
			name:    "zygote command and registry",
			mod:     func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Registry = "ghcr.io/a/z" },
			wantErr: "exactly one of zygote.command and zygote.registry must be set",
		},
		{
			name: "zygote registry with a tag",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/a/z:latest"}
			},
			wantErr: "spec.zygote.registry: Invalid value",
		},
		{
			name: "zygote registry stored with an https scheme",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "https://ghcr.io/a/z"}
			},
			wantErr: "spec.zygote.registry: Invalid value",
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
			name:    "workingDir set",
			mod:     ctrMod(func(c *corev1.Container) { c.WorkingDir = "/srv" }),
			wantErr: "spec.podTemplate.spec.containers[0].workingDir: Forbidden",
		},
		{
			name: "lifecycle hook set",
			mod: ctrMod(func(c *corev1.Container) {
				c.Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh"}}}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].lifecycle",
		},
		{
			name:    "command and lifecycle hook together report the command",
			mod:     commandAndHook,
			wantErr: "containers[0].command: Forbidden",
		},
		{
			name:    "command and lifecycle hook together report the hook",
			mod:     commandAndHook,
			wantErr: "containers[0].lifecycle: Forbidden",
		},
		{
			name: "envFrom source",
			mod: ctrMod(func(c *corev1.Container) {
				c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "trust"}}}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].envFrom",
		},
		{
			name: "host aliases",
			mod: podMod(func(s *corev1.PodSpec) {
				s.HostAliases = []corev1.HostAlias{{IP: "10.0.0.9", Hostnames: []string{"issuer.example"}}}
			}),
			wantErr: "spec.podTemplate.spec.hostAliases",
		},
		{
			name:    "DNS config",
			mod:     podMod(func(s *corev1.PodSpec) { s.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"10.0.0.9"}} }),
			wantErr: "spec.podTemplate.spec.dnsConfig",
		},
		{
			name:    "runtime class",
			mod:     podMod(func(s *corev1.PodSpec) { s.RuntimeClassName = ptr.To("kata") }),
			wantErr: "spec.podTemplate.spec.runtimeClassName",
		},
		{
			name: "Pod SELinux options",
			mod: podMod(func(s *corev1.PodSpec) {
				s.SecurityContext = &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"}}
			}),
			wantErr: "spec.podTemplate.spec.securityContext.seLinuxOptions",
		},
		{
			name: "Pod Windows options",
			mod: podMod(func(s *corev1.PodSpec) {
				s.SecurityContext = &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{}}
			}),
			wantErr: "spec.podTemplate.spec.securityContext.windowsOptions",
		},
		{
			name: "container SELinux options",
			mod: ctrMod(func(c *corev1.Container) {
				c.SecurityContext = &corev1.SecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].securityContext.seLinuxOptions",
		},
		{
			name: "container Windows options",
			mod: ctrMod(func(c *corev1.Container) {
				c.SecurityContext = &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].securityContext.windowsOptions",
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
			name: "volume named fiberd-helper",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-helper", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name: Duplicate value",
		},
		{
			name: "volume named fiberd-state",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-state", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "volume named fiberd-grants",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-grants", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "volume named fiberd-tls",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-tls", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "volume named fiberd-grant-token",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-grant-token", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "volume named fiberd-seal-key",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "fiberd-seal-key", VolumeSource: emptyDir}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].name",
		},
		{
			name: "hostPath volume",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0]: " + sourceWhy,
		},
		{
			name: "image volume",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "img", VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "example.com/img"}}}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0]: " + sourceWhy,
		},
		{
			name: "projected service account token",
			mod: podMod(func(s *corev1.PodSpec) {
				s.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
				}}}}
			}),
			wantErr: "spec.podTemplate.spec.volumes[0].projected: Forbidden: must not project a service account token, which fibers could read",
		},
		{
			name: "mount of the grants volume elsewhere",
			mod: ctrMod(func(c *corev1.Container) {
				c.VolumeMounts = []corev1.VolumeMount{{Name: "fiberd-grants", MountPath: "/data/leak"}}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].volumeMounts[0].name",
		},
		{
			name:    "empty mount path",
			mod:     ctrMod(func(c *corev1.Container) { c.VolumeMounts = []corev1.VolumeMount{{Name: "cache"}} }),
			wantErr: "volumeMounts[0].mountPath: Invalid value",
		},
		{
			name:    "empty device path",
			mod:     ctrMod(func(c *corev1.Container) { c.VolumeDevices = []corev1.VolumeDevice{{Name: "disk"}} }),
			wantErr: "volumeDevices[0].devicePath: Invalid value",
		},
		{
			name:    "env POD_NAME",
			mod:     ctrMod(func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "POD_NAME", Value: "forged"}} }),
			wantErr: "spec.podTemplate.spec.containers[0].env[0].name",
		},
		{
			name:    "env SSL_CERT_FILE",
			mod:     ctrMod(func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: "/ca/ca.pem"}} }),
			wantErr: `env[0].name: Forbidden: "SSL_CERT_FILE" ` + envWhy,
		},
		{
			name:    "env LD_PRELOAD",
			mod:     ctrMod(func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/x.so"}} }),
			wantErr: `env[0].name: Forbidden: "LD_PRELOAD" ` + envWhy,
		},
		{
			name:    "env GODEBUG",
			mod:     ctrMod(func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "GODEBUG", Value: "x"}} }),
			wantErr: `env[0].name: Forbidden: "GODEBUG" ` + envWhy,
		},
		{
			name: "reserved label under fiberd.io",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Labels = map[string]string{v1alpha1.LabelGrantUID: "forged"}
			},
			wantErr: "spec.podTemplate.metadata.labels[fiberd.io/grant-uid]: Forbidden: is reserved",
		},
		{
			name: "reserved label under a fiberd.io subdomain",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Labels = map[string]string{"zygote.fiberd.io/name": "x"}
			},
			wantErr: "spec.podTemplate.metadata.labels[zygote.fiberd.io/name]: Forbidden: is reserved",
		},
		{
			name: "forged hash annotation",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Annotations = map[string]string{AnnotationGrantHash: "forged"}
			},
			wantErr: "spec.podTemplate.metadata.annotations[fiberd.io/grant-hash]: Forbidden: is reserved",
		},
		{
			name: "reserved Multus network annotation",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": "host"}
			},
			wantErr: "spec.podTemplate.metadata.annotations[k8s.v1.cni.cncf.io/networks]: Forbidden: is reserved",
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
				c.Args = []string{"-delta-registry=example.com/d", "-issuer=https://evil.example"}
			}),
			wantErr: "spec.podTemplate.spec.containers[0].args[1]",
		},
		{
			name:    "tenant template for the grant's digest",
			mod:     ctrMod(func(c *corev1.Container) { c.Args = []string{"-template=" + testDigest + "=/bin/sh"} }),
			wantErr: "spec.podTemplate.spec.containers[0].args[0]: Forbidden: -template is not a flag a tenant may set",
		},
		{
			name:    "tenant pod-name flag",
			mod:     ctrMod(func(c *corev1.Container) { c.Args = []string{"-pod-name=forged"} }),
			wantErr: "spec.podTemplate.spec.containers[0].args[0]: Forbidden: -pod-name is not a flag a tenant may set",
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
			service := testServiceName
			if tc.noService {
				service = ""
			}
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			_, err := Build(cg, image, service, opts)
			if err == nil {
				t.Fatalf("Build succeeded, want an error holding %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Build error = %q, want it to hold %q", err, tc.wantErr)
			}
		})
	}
}
