{{- define "mailvault.name" -}}
{{- printf "%s-mailvault" .Release.Name | trunc 45 | trimSuffix "-" -}}
{{- end -}}
{{- define "mailvault.image" -}}
{{- .repository -}}:{{- default "latest" (default .defaultTag .tag) -}}{{- if .digest -}}@{{ .digest }}{{- end -}}
{{- end -}}
{{- define "mailvault.claim" -}}
{{- if .settings.existingClaim -}}{{ .settings.existingClaim }}{{- else -}}{{ .name }}{{- end -}}
{{- end -}}
