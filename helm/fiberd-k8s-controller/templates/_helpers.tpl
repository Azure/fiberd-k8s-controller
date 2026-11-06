{{- /* The controller image by digest, since the chart has no tag value. */ -}}
{{- define "fiberd-k8s-controller.image" -}}
{{- $repository := required "image.repository is required" .Values.image.repository -}}
{{- $digest := required "image.digest is required, since the chart names the controller image by digest" .Values.image.digest -}}
{{- printf "%s@%s" $repository $digest -}}
{{- end -}}
