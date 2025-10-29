/*
Copyright 2025 The Kubernetes Authors.

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

package kustomize

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	kindNamespace          = "Namespace"
	kindCertificate        = "Certificate"
	kindService            = "Service"
	kindServiceAccount     = "ServiceAccount"
	kindRole               = "Role"
	kindClusterRole        = "ClusterRole"
	kindRoleBinding        = "RoleBinding"
	kindClusterRoleBinding = "ClusterRoleBinding"
	kindServiceMonitor     = "ServiceMonitor"
	kindIssuer             = "Issuer"
	kindValidatingWebhook  = "ValidatingWebhookConfiguration"
	kindMutatingWebhook    = "MutatingWebhookConfiguration"

	// API versions
	apiVersionCertManager = "cert-manager.io/v1"
	apiVersionMonitoring  = "monitoring.coreos.com/v1"

	chartNameTemplate = "chart.name"
)

// HelmTemplater handles converting YAML content to Helm templates
type HelmTemplater struct {
	projectName string
}

// NewHelmTemplater creates a new Helm templater
func NewHelmTemplater(projectName string) *HelmTemplater {
	return &HelmTemplater{
		projectName: projectName,
	}
}

// ApplyHelmSubstitutions converts YAML content to use Helm template syntax
func (t *HelmTemplater) ApplyHelmSubstitutions(yamlContent string, resource *unstructured.Unstructured) string {
	// Apply conditional wrappers first
	yamlContent = t.addConditionalWrappers(yamlContent, resource)

	// Apply general project name substitutions
	yamlContent = t.substituteProjectNames(yamlContent, resource)

	// Apply namespace substitutions
	yamlContent = t.substituteNamespace(yamlContent, resource)

	// Template ServiceMonitor metadata.name to be chart.name-scoped for consistency
	yamlContent = t.templateServiceMonitorNames(yamlContent, resource)

	// Apply cert-manager and webhook-specific templating AFTER other substitutions
	yamlContent = t.substituteCertManagerReferences(yamlContent, resource)

	// Apply labels and annotations from Helm chart
	yamlContent = t.addHelmLabelsAndAnnotations(yamlContent, resource)

	// Apply resource-specific substitutions
	yamlContent = t.substituteRBACValues(yamlContent)

	// Apply deployment-specific templating
	if resource.GetKind() == "Deployment" {
		yamlContent = t.templateDeploymentFields(yamlContent)

		// Apply conditional logic for cert-manager related fields in deployments
		yamlContent = t.makeContainerArgsConditional(yamlContent)
		yamlContent = t.makeWebhookVolumeMountsConditional(yamlContent)
		yamlContent = t.makeWebhookVolumesConditional(yamlContent)
		yamlContent = t.makeMetricsVolumeMountsConditional(yamlContent)
		yamlContent = t.makeMetricsVolumesConditional(yamlContent)
	}

	// Final tidy-up: avoid accidental blank lines after Helm if-block starts
	// Some replacements may introduce an empty line between a `{{- if ... }}`
	// and the following content; collapse that to ensure consistent formatting.
	yamlContent = t.collapseBlankLineAfterIf(yamlContent)

	return yamlContent
}

// substituteProjectNames keeps original YAML as much as possible - only add Helm templating
func (t *HelmTemplater) substituteProjectNames(yamlContent string, _ *unstructured.Unstructured) string {
	return yamlContent
}

// substituteNamespace replaces hardcoded namespace references with Release.Namespace
func (t *HelmTemplater) substituteNamespace(yamlContent string, resource *unstructured.Unstructured) string {
	hardcodedNamespace := t.projectName + "-system"
	namespaceTemplate := "{{ .Release.Namespace }}"

	// Replace hardcoded namespace references everywhere, including in the Namespace resource
	// so that metadata.name becomes the Helm release namespace.
	yamlContent = strings.ReplaceAll(yamlContent, hardcodedNamespace, namespaceTemplate)

	// Replace service DNS name placeholders in certificates
	if resource.GetKind() == kindCertificate {
		yamlContent = t.substituteCertificateDNSNames(yamlContent, resource)
	}

	return yamlContent
}

// substituteCertificateDNSNames replaces hardcoded DNS names in certificates with proper service templates
func (t *HelmTemplater) substituteCertificateDNSNames(yamlContent string, resource *unstructured.Unstructured) string {
	name := resource.GetName()

	// Replace service names with templated ones based on certificate type
	if strings.Contains(name, "metrics-cert") || strings.Contains(name, "metrics") {
		// Metrics certificates should point to metrics service
		// Use chart.name based service naming for consistency
		metricsServiceTemplate := "{{ include \"chart.serviceName\" " +
			"(dict \"suffix\" \"controller-manager-metrics-service\" \"context\" .) }}"
		metricsServiceFQDN := metricsServiceTemplate + ".{{ include \"chart.namespaceName\" . }}.svc"
		metricsServiceFQDNCluster := metricsServiceTemplate +
			".{{ include \"chart.namespaceName\" . }}.svc.cluster.local"

		// Replace placeholders
		yamlContent = strings.ReplaceAll(yamlContent, "SERVICE_NAME.SERVICE_NAMESPACE.svc", metricsServiceFQDN)
		yamlContent = strings.ReplaceAll(yamlContent,
			"SERVICE_NAME.SERVICE_NAMESPACE.svc.cluster.local", metricsServiceFQDNCluster)

		// Also replace hardcoded service names
		hardcodedMetricsService := t.projectName + "-controller-manager-metrics-service"
		yamlContent = strings.ReplaceAll(yamlContent, hardcodedMetricsService, metricsServiceTemplate)
	}

	// Replace hardcoded issuer reference with templated one
	hardcodedIssuer := t.projectName + "-selfsigned-issuer"
	templatedIssuer := "{{ include \"" + chartNameTemplate + "\" . }}-selfsigned-issuer"
	yamlContent = strings.ReplaceAll(yamlContent, hardcodedIssuer, templatedIssuer)

	return yamlContent
}

// substituteCertManagerReferences applies cert-manager and webhook-specific template substitutions
func (t *HelmTemplater) substituteCertManagerReferences(yamlContent string, _ *unstructured.Unstructured) string {
	return yamlContent
}

// templateServiceNames ensures Service metadata.name uses Helm helpers for release-scoped naming.
// It converts hardcoded names like "<project>-webhook-service" or
// "<project>-controller-manager-metrics-service" to:
//
//	name: {{ include "chart.serviceName" (dict "suffix" "<suffix>" "context" .) }}
// NOTE: Service names are intentionally left as project-scoped (no release prefix)
// to match existing samples and e2e tests that refer to fixed service names.

// templateServiceMonitorNames ensures ServiceMonitor metadata.name uses chart.name-based naming for consistency
// with existing tests and samples. It converts names like "controller-manager-metrics-monitor" or
// "<project>-controller-manager-metrics-monitor" to:
//
//	name: {{ include "chart.name" . }}-controller-manager-metrics-monitor
func (t *HelmTemplater) templateServiceMonitorNames(yamlContent string, resource *unstructured.Unstructured) string {
	if resource.GetKind() != kindServiceMonitor {
		return yamlContent
	}

	origName := resource.GetName()
	if origName == "" {
		return yamlContent
	}

	// Normalize suffix by stripping the project prefix if present
	suffix := origName
	prefix := t.projectName + "-"
	if strings.HasPrefix(origName, prefix) {
		suffix = strings.TrimPrefix(origName, prefix)
	}

	// Only template if the intended target follows the conventional suffix
	// This keeps any custom user-provided names intact
	// For default scaffolding, suffix is typically "controller-manager-metrics-monitor"
	templated := "{{ include \"chart.name\" . }}-" + suffix

	nameRe := regexp.MustCompile("(?m)^([\t ]*)name:\\s*" + regexp.QuoteMeta(origName) + "\\s*$")
	yamlContent = nameRe.ReplaceAllString(yamlContent, "${1}name: "+templated)

	return yamlContent
}

// addHelmLabelsAndAnnotations replaces kustomize managed-by labels with Helm equivalents
func (t *HelmTemplater) addHelmLabelsAndAnnotations(yamlContent string, _ *unstructured.Unstructured) string {
	// Replace app.kubernetes.io/managed-by: kustomize with Helm template
	// Use regex to handle different whitespace patterns
	managedByRegex := regexp.MustCompile(`(\s*)app\.kubernetes\.io/managed-by:\s+kustomize`)
	yamlContent = managedByRegex.ReplaceAllString(yamlContent, "${1}app.kubernetes.io/managed-by: {{ .Release.Service }}")

	return yamlContent
}

// substituteRBACValues applies RBAC-specific template substitutions
func (t *HelmTemplater) substituteRBACValues(yamlContent string) string {
	return yamlContent
}

// templateDeploymentFields converts deployment-specific fields to Helm templates
func (t *HelmTemplater) templateDeploymentFields(yamlContent string) string {
	// Template configuration fields
	yamlContent = t.templateImageReference(yamlContent)
	yamlContent = t.templateEnvironmentVariables(yamlContent)
	yamlContent = t.templatePodSecurityContext(yamlContent)
	yamlContent = t.templateContainerSecurityContext(yamlContent)
	yamlContent = t.templateResources(yamlContent)
	yamlContent = t.templateSecurityContexts(yamlContent)
	yamlContent = t.templateVolumeMounts(yamlContent)
	yamlContent = t.templateVolumes(yamlContent)
	yamlContent = t.templateControllerManagerArgs(yamlContent)

	return yamlContent
}

// templateEnvironmentVariables exposes environment variables via values.yaml
func (t *HelmTemplater) templateEnvironmentVariables(yamlContent string) string {
	if !strings.Contains(yamlContent, "name: manager") {
		return yamlContent
	}

	lines := strings.Split(yamlContent, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "env:" {
			continue
		}

		indentStr, indentLen := leadingWhitespace(lines[i])
		end := i + 1
		for ; end < len(lines); end++ {
			trimmed := strings.TrimSpace(lines[end])
			if trimmed == "" {
				break
			}
			lineIndent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
			if lineIndent < indentLen {
				break
			}
			if lineIndent == indentLen && !strings.HasPrefix(trimmed, "-") {
				break
			}
		}

		if i+1 < len(lines) && strings.Contains(lines[i+1], ".Values.controllerManager.env") {
			return yamlContent
		}

		childIndent := indentStr + "  "
		childIndentWidth := strconv.Itoa(len(childIndent))

		block := []string{
			indentStr + "env:",
			childIndent + "{{- if .Values.controllerManager.env }}",
			childIndent + "{{- toYaml .Values.controllerManager.env | nindent " + childIndentWidth + " }}",
			childIndent + "{{- else }}",
			childIndent + "[]",
			childIndent + "{{- end }}",
		}

		newLines := append([]string{}, lines[:i]...)
		newLines = append(newLines, block...)
		newLines = append(newLines, lines[end:]...)
		return strings.Join(newLines, "\n")
	}

	return yamlContent
}

// templateResources converts resource sections to Helm templates
func (t *HelmTemplater) templateResources(yamlContent string) string {
	if !strings.Contains(yamlContent, "name: manager") || !strings.Contains(yamlContent, "resources:") {
		return yamlContent
	}

	lines := strings.Split(yamlContent, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "resources:" {
			continue
		}

		indentStr, indentLen := leadingWhitespace(lines[i])
		end := i + 1
		for ; end < len(lines); end++ {
			trimmed := strings.TrimSpace(lines[end])
			if trimmed == "" {
				break
			}
			lineIndent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
			if lineIndent < indentLen {
				break
			}
			// stop at same-level keys that are not part of the resources mapping
			if lineIndent == indentLen && !strings.Contains(trimmed, ":") {
				break
			}
			if lineIndent == indentLen && strings.HasSuffix(trimmed, ":") {
				break
			}
		}

		if i+1 < len(lines) && strings.Contains(lines[i+1], ".Values.controllerManager.resources") {
			return yamlContent
		}

		childIndent := indentStr + "  "
		childIndentWidth := strconv.Itoa(len(childIndent))

		block := []string{
			indentStr + "resources:",
			childIndent + "{{- if .Values.controllerManager.resources }}",
			childIndent + "{{- toYaml .Values.controllerManager.resources | nindent " + childIndentWidth + " }}",
			childIndent + "{{- else }}",
			childIndent + "{}",
			childIndent + "{{- end }}",
		}

		newLines := append([]string{}, lines[:i]...)
		newLines = append(newLines, block...)
		newLines = append(newLines, lines[end:]...)
		return strings.Join(newLines, "\n")
	}

	return yamlContent
}

// templateSecurityContexts preserves security contexts from kustomize output
func (t *HelmTemplater) templateSecurityContexts(yamlContent string) string {
	// Security contexts are preserved as-is from the kustomize output to maintain
	// the exact security configuration without interfering with other container fields
	return yamlContent
}

// templateVolumeMounts converts volumeMounts sections to keep them as-is since they're webhook-specific
func (t *HelmTemplater) templateVolumeMounts(yamlContent string) string {
	// For webhook volumeMounts, we keep them as-is since they're required for webhook functionality
	// They will be conditionally included based on webhook configuration
	return yamlContent
}

// templateVolumes converts volumes sections to keep them as-is since they're webhook-specific
func (t *HelmTemplater) templateVolumes(yamlContent string) string {
	// For webhook volumes, we keep them as-is since they're required for webhook functionality
	// They will be conditionally included based on webhook configuration
	return yamlContent
}

// templatePodSecurityContext exposes podSecurityContext via values.yaml
func (t *HelmTemplater) templatePodSecurityContext(yamlContent string) string {
	if !strings.Contains(yamlContent, "securityContext:") {
		return yamlContent
	}

	lines := strings.Split(yamlContent, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "securityContext:" {
			continue
		}

		indentStr, indentLen := leadingWhitespace(lines[i])
		end := i + 1
		for ; end < len(lines); end++ {
			trimmed := strings.TrimSpace(lines[end])
			if trimmed == "" {
				break
			}
			lineIndent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
			if lineIndent <= indentLen {
				break
			}
		}

		if end >= len(lines) {
			break
		}

		if !strings.HasPrefix(strings.TrimSpace(lines[end]), "serviceAccountName:") {
			continue
		}

		if i+1 < len(lines) && strings.Contains(lines[i+1], ".Values.controllerManager.podSecurityContext") {
			return yamlContent
		}

		childIndent := indentStr + "  "
		childIndentWidth := strconv.Itoa(len(childIndent))

		block := []string{
			indentStr + "securityContext:",
			childIndent + "{{- if .Values.controllerManager.podSecurityContext }}",
			childIndent + "{{- toYaml .Values.controllerManager.podSecurityContext | nindent " + childIndentWidth + " }}",
			childIndent + "{{- else }}",
			childIndent + "{}",
			childIndent + "{{- end }}",
		}

		newLines := append([]string{}, lines[:i]...)
		newLines = append(newLines, block...)
		newLines = append(newLines, lines[end:]...)
		return strings.Join(newLines, "\n")
	}

	return yamlContent
}

// templateContainerSecurityContext exposes container securityContext via values.yaml
func (t *HelmTemplater) templateContainerSecurityContext(yamlContent string) string {
	if !strings.Contains(yamlContent, "name: manager") || !strings.Contains(yamlContent, "securityContext:") {
		return yamlContent
	}

	lines := strings.Split(yamlContent, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "securityContext:" {
			continue
		}

		indentStr, indentLen := leadingWhitespace(lines[i])
		end := i + 1
		for ; end < len(lines); end++ {
			trimmed := strings.TrimSpace(lines[end])
			if trimmed == "" {
				break
			}
			lineIndent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
			if lineIndent <= indentLen {
				break
			}
		}

		if end >= len(lines) {
			break
		}

		if strings.HasPrefix(strings.TrimSpace(lines[end]), "serviceAccountName:") {
			continue
		}

		lookAheadEnd := end + 5
		if lookAheadEnd > len(lines) {
			lookAheadEnd = len(lines)
		}
		joined := strings.Join(lines[i:lookAheadEnd], "\n")
		if strings.Contains(joined, ".Values.controllerManager.securityContext") {
			return yamlContent
		}

		childIndent := indentStr + "  "
		childIndentWidth := strconv.Itoa(len(childIndent))

		block := []string{
			indentStr + "securityContext:",
			childIndent + "{{- if .Values.controllerManager.securityContext }}",
			childIndent + "{{- toYaml .Values.controllerManager.securityContext | nindent " + childIndentWidth + " }}",
			childIndent + "{{- else }}",
			childIndent + "{}",
			childIndent + "{{- end }}",
		}

		newLines := append([]string{}, lines[:i]...)
		newLines = append(newLines, block...)
		newLines = append(newLines, lines[end:]...)
		return strings.Join(newLines, "\n")
	}

	return yamlContent
}

func leadingWhitespace(line string) (string, int) {
	trimmed := strings.TrimLeft(line, " \t")
	indentLen := len(line) - len(trimmed)
	return line[:indentLen], indentLen
}

// templateControllerManagerArgs exposes controller manager args via values.yaml while keeping core defaults
func (t *HelmTemplater) templateControllerManagerArgs(yamlContent string) string {
	if !strings.Contains(yamlContent, "name: manager") {
		return yamlContent
	}

	argsPattern := regexp.MustCompile(`(?m)([ \t]+)args:\n((?:[ \t]+-.*\n)+)`)
	loc := argsPattern.FindStringSubmatchIndex(yamlContent)
	if loc == nil {
		return yamlContent
	}

	match := yamlContent[loc[0]:loc[1]]
	if strings.Contains(match, ".Values.controllerManager.args") {
		return yamlContent
	}

	indent := yamlContent[loc[2]:loc[3]]
	itemsBlock := yamlContent[loc[4]:loc[5]]

	itemIndent := indent + "  "
	lines := strings.Split(itemsBlock, "\n")
	var (
		metricsLine    string
		metricsIndent  string
		healthLine     string
		preservedLines []string
	)

	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if itemIndent == indent+"  " {
			if idx := strings.Index(line, "-"); idx > 0 {
				itemIndent = line[:idx]
			}
		}

		switch {
		case strings.Contains(trimmed, "--metrics-bind-address"):
			metricsLine = line
			if idx := strings.Index(line, "-"); idx > 0 {
				metricsIndent = line[:idx]
			}
		case strings.Contains(trimmed, "--health-probe-bind-address"):
			healthLine = line
		case strings.Contains(trimmed, "--webhook-cert-path"),
			strings.Contains(trimmed, "--metrics-cert-path"):
			preservedLines = append(preservedLines, line)
		default:
			// Remaining args will be handled through values.yaml
		}
	}

	var builder strings.Builder
	builder.WriteString(indent)
	builder.WriteString("args:\n")

	if metricsLine != "" {
		if metricsIndent == "" {
			metricsIndent = itemIndent
		}
		builder.WriteString(metricsIndent)
		builder.WriteString("{{- if .Values.metrics.enable }}\n")
		builder.WriteString(metricsLine)
		builder.WriteString("\n")
		builder.WriteString(metricsIndent)
		builder.WriteString("{{- else }}\n")
		builder.WriteString(metricsIndent)
		builder.WriteString("# Bind to :0 to disable the controller-runtime managed metrics server\n")
		builder.WriteString(metricsIndent)
		builder.WriteString("- --metrics-bind-address=0\n")
		builder.WriteString(metricsIndent)
		builder.WriteString("{{- end }}\n")
	}
	if healthLine != "" {
		builder.WriteString(healthLine)
		builder.WriteString("\n")
	}

	builder.WriteString(itemIndent)
	builder.WriteString("{{- range .Values.controllerManager.args }}\n")
	builder.WriteString(itemIndent)
	builder.WriteString("- {{ . }}\n")
	builder.WriteString(itemIndent)
	builder.WriteString("{{- end }}\n")

	for _, line := range preservedLines {
		builder.WriteString(line)
		builder.WriteString("\n")
	}

	newBlock := strings.TrimRight(builder.String(), "\n") + "\n"

	return yamlContent[:loc[0]] + newBlock + yamlContent[loc[1]:]
}

// templateImageReference converts hardcoded image references to Helm templates
func (t *HelmTemplater) templateImageReference(yamlContent string) string {
	if !strings.Contains(yamlContent, "name: manager") {
		return yamlContent
	}

	lines := strings.Split(yamlContent, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}

		if strings.Contains(lines[i], ".Values.controllerManager.image.repository") {
			return yamlContent
		}

		indentStr, indentLen := leadingWhitespace(lines[i])

		end := i + 1
		for ; end < len(lines); end++ {
			nextTrimmed := strings.TrimSpace(lines[end])
			if nextTrimmed == "" {
				break
			}
			lineIndent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
			if lineIndent <= indentLen {
				break
			}
			// Stop when we reach a sibling key like env:, args:, etc.
			if lineIndent == indentLen+2 && strings.HasSuffix(nextTrimmed, ":") {
				if strings.Contains(nextTrimmed, "imagePullPolicy") {
					continue
				}
				break
			}
		}

		// Remove any existing imagePullPolicy line inside the block
		blockLines := lines[i+1 : end]
		filtered := make([]string, 0, len(blockLines))
		for _, line := range blockLines {
			if strings.Contains(strings.TrimSpace(line), "imagePullPolicy") {
				continue
			}
			filtered = append(filtered, line)
		}
		lines = append(lines[:i+1], append(filtered, lines[end:]...)...)
		end = i + 1 + len(filtered)

		//nolint:lll
		imageLine := indentStr + "image: \"{{ .Values.controllerManager.image.repository }}:{{ .Values.controllerManager.image.tag }}\""
		pullPolicyLine := indentStr + "imagePullPolicy: {{ .Values.controllerManager.image.pullPolicy }}"

		remainder := lines[end:]
		if len(remainder) > 0 && strings.HasPrefix(strings.TrimSpace(remainder[0]), "imagePullPolicy:") {
			remainder = remainder[1:]
		}

		newLines := append([]string{}, lines[:i]...)
		newLines = append(newLines, imageLine, pullPolicyLine)
		newLines = append(newLines, remainder...)
		return strings.Join(newLines, "\n")
	}

	return yamlContent
}

// makeWebhookAnnotationsConditional makes only cert-manager annotations conditional, not the entire webhook
func (t *HelmTemplater) makeWebhookAnnotationsConditional(yamlContent string) string {
	// Find cert-manager.io/inject-ca-from annotation and make it conditional
	if strings.Contains(yamlContent, "cert-manager.io/inject-ca-from") {
		// Replace the cert-manager annotation with conditional wrapper
		certManagerPattern := regexp.MustCompile(`(\s+)cert-manager\.io/inject-ca-from:\s*[^\n]+`)
		yamlContent = certManagerPattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			// Extract the indentation
			indentMatch := regexp.MustCompile(`^(\s+)`).FindStringSubmatch(match)
			indent := ""
			if len(indentMatch) > 1 {
				indent = indentMatch[1]
			}

			// Extract the annotation line with proper indentation
			annotationLine := strings.TrimSpace(match)

			return fmt.Sprintf("%s{{- if .Values.certManager.enable }}\n%s%s\n%s{{- end }}",
				indent, indent, annotationLine, indent)
		})
	}

	return yamlContent
}

// makeContainerArgsConditional makes webhook-cert-path and metrics-cert-path args conditional on certManager.enable
func (t *HelmTemplater) makeContainerArgsConditional(yamlContent string) string {
	// Make webhook-cert-path arg conditional on certManager.enable
	if strings.Contains(yamlContent, "--webhook-cert-path") {
		// Match only spaces/tabs for indent to avoid consuming the newline
		webhookArgPattern := regexp.MustCompile(`([ \t]+)-\s*--webhook-cert-path=[^\n]*`)
		yamlContent = webhookArgPattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			indentMatch := regexp.MustCompile(`^(\s+)`).FindStringSubmatch(match)
			indent := ""
			if len(indentMatch) > 1 {
				indent = indentMatch[1]
			}

			argLine := strings.TrimSpace(match)
			return fmt.Sprintf("%s{{- if .Values.certManager.enable }}\n%s%s\n%s{{- end }}",
				indent, indent, argLine, indent)
		})
	}

	// Make metrics-cert-path arg conditional on certManager.enable AND metrics.enable
	if strings.Contains(yamlContent, "--metrics-cert-path") {
		// Match only spaces/tabs for indent to avoid consuming the newline
		metricsArgPattern := regexp.MustCompile(`([ \t]+)-\s*--metrics-cert-path=[^\n]*`)
		yamlContent = metricsArgPattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			indentMatch := regexp.MustCompile(`^(\s+)`).FindStringSubmatch(match)
			indent := ""
			if len(indentMatch) > 1 {
				indent = indentMatch[1]
			}

			argLine := strings.TrimSpace(match)
			return fmt.Sprintf("%s{{- if and .Values.certManager.enable .Values.metrics.enable }}\n%s%s\n%s{{- end }}",
				indent, indent, argLine, indent)
		})
	}

	return yamlContent
}

// makeWebhookVolumesConditional makes webhook volumes conditional on certManager.enable
func (t *HelmTemplater) makeWebhookVolumesConditional(yamlContent string) string {
	// Make webhook volumes conditional on certManager.enable
	if strings.Contains(yamlContent, "webhook-certs") && strings.Contains(yamlContent, "secretName: webhook-server-cert") {
		// Match only spaces/tabs for indent to avoid consuming the newline
		volumePattern := regexp.MustCompile(`([ \t]+)-\s*name:\s*webhook-certs[\s\S]*?secretName:\s*webhook-server-cert`)
		yamlContent = volumePattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			lines := strings.Split(match, "\n")
			if len(lines) > 0 {
				indent := ""
				if len(lines[0]) > 0 && lines[0][0] == ' ' {
					// Count leading spaces
					for _, char := range lines[0] {
						if char == ' ' {
							indent += " "
						} else {
							break
						}
					}
				}

				// Reconstruct the block with conditional wrapper
				result := fmt.Sprintf("%s{{- if .Values.certManager.enable }}\n", indent)
				for _, line := range lines {
					result += line + "\n"
				}
				result += fmt.Sprintf("%s{{- end }}", indent)
				return result
			}
			return match
		})
	}

	return yamlContent
}

// makeWebhookVolumeMountsConditional makes webhook volumeMounts conditional on certManager.enable
func (t *HelmTemplater) makeWebhookVolumeMountsConditional(yamlContent string) string {
	// Make webhook volumeMounts conditional on certManager.enable
	webhookCertsPath := "/tmp/k8s-webhook-server/serving-certs"
	if strings.Contains(yamlContent, "webhook-certs") && strings.Contains(yamlContent, webhookCertsPath) {
		// Match only spaces/tabs for indent to avoid consuming the newline
		mountPattern := regexp.MustCompile(
			`([ \t]+)-\s*mountPath:\s*/tmp/k8s-webhook-server/serving-certs[\s\S]*?readOnly:\s*true`)
		yamlContent = mountPattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			lines := strings.Split(match, "\n")
			if len(lines) > 0 {
				indent := ""
				if len(lines[0]) > 0 && lines[0][0] == ' ' {
					// Count leading spaces
					for _, char := range lines[0] {
						if char == ' ' {
							indent += " "
						} else {
							break
						}
					}
				}

				// Reconstruct the block with conditional wrapper
				result := fmt.Sprintf("%s{{- if .Values.certManager.enable }}\n", indent)
				for _, line := range lines {
					result += line + "\n"
				}
				result += fmt.Sprintf("%s{{- end }}", indent)
				return result
			}
			return match
		})
	}

	return yamlContent
}

// makeMetricsVolumesConditional makes metrics volumes conditional on certManager.enable AND metrics.enable
func (t *HelmTemplater) makeMetricsVolumesConditional(yamlContent string) string {
	// Make metrics volumes conditional on certManager.enable AND metrics.enable
	if strings.Contains(yamlContent, "metrics-certs") && strings.Contains(yamlContent, "secretName: metrics-server-cert") {
		// Match only spaces/tabs for indent to avoid consuming the newline
		volumePattern := regexp.MustCompile(`([ \t]+)-\s*name:\s*metrics-certs[\s\S]*?secretName:\s*metrics-server-cert`)
		yamlContent = volumePattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			lines := strings.Split(match, "\n")
			if len(lines) > 0 {
				indent := ""
				if len(lines[0]) > 0 && lines[0][0] == ' ' {
					// Count leading spaces
					for _, char := range lines[0] {
						if char == ' ' {
							indent += " "
						} else {
							break
						}
					}
				}

				// Reconstruct the block with conditional wrapper
				result := fmt.Sprintf("%s{{- if and .Values.certManager.enable .Values.metrics.enable }}\n", indent)
				for _, line := range lines {
					result += line + "\n"
				}
				result += fmt.Sprintf("%s{{- end }}", indent)
				return result
			}
			return match
		})
	}

	return yamlContent
}

// injectCommonLabels adds a Helm template snippet to append user-provided common labels
// (.Values.commonLabels) to every metadata.labels block while preserving indentation.
// It avoids duplicate insertion by checking for an existing snippet nearby.
// no common labels injection; labels come from kustomize manifests

// makeMetricsVolumeMountsConditional makes metrics volumeMounts conditional on certManager.enable AND metrics.enable
func (t *HelmTemplater) makeMetricsVolumeMountsConditional(yamlContent string) string {
	// Make metrics volumeMounts conditional on certManager.enable AND metrics.enable
	metricsCertsPath := "/tmp/k8s-metrics-server/metrics-certs"
	if strings.Contains(yamlContent, "metrics-certs") && strings.Contains(yamlContent, metricsCertsPath) {
		// Match only spaces/tabs for indent to avoid consuming the newline
		mountPattern := regexp.MustCompile(
			`([ \t]+)-\s*mountPath:\s*/tmp/k8s-metrics-server/metrics-certs[\s\S]*?readOnly:\s*true`)
		yamlContent = mountPattern.ReplaceAllStringFunc(yamlContent, func(match string) string {
			lines := strings.Split(match, "\n")
			if len(lines) > 0 {
				indent := ""
				if len(lines[0]) > 0 && lines[0][0] == ' ' {
					// Count leading spaces
					for _, char := range lines[0] {
						if char == ' ' {
							indent += " "
						} else {
							break
						}
					}
				}

				// Reconstruct the block with conditional wrapper
				result := fmt.Sprintf("%s{{- if and .Values.certManager.enable .Values.metrics.enable }}\n", indent)
				for _, line := range lines {
					result += line + "\n"
				}
				result += fmt.Sprintf("%s{{- end }}", indent)
				return result
			}
			return match
		})
	}

	return yamlContent
}

// addConditionalWrappers adds conditional Helm logic based on resource type
func (t *HelmTemplater) addConditionalWrappers(yamlContent string, resource *unstructured.Unstructured) string {
	kind := resource.GetKind()
	apiVersion := resource.GetAPIVersion()
	name := resource.GetName()

	switch {
	case kind == kindNamespace:
		return ""
	case kind == "CustomResourceDefinition":
		// CRDs need crd.enable condition
		return fmt.Sprintf("{{- if .Values.crd.enable }}\n%s{{- end }}\n", yamlContent)
	case kind == kindCertificate && apiVersion == apiVersionCertManager:
		// Handle different certificate types
		if strings.Contains(name, "metrics-cert") || strings.Contains(name, "metrics") {
			// Metrics certificates need both certManager and metrics enabled
			return fmt.Sprintf("{{- if and .Values.certManager.enable .Values.metrics.enable }}\n%s{{- end }}\n",
				yamlContent)
		}
		// Other certificates (webhook serving certs) only need certManager enabled
		return fmt.Sprintf("{{- if .Values.certManager.enable }}\n%s{{- end }}", yamlContent)
	case kind == kindIssuer && apiVersion == apiVersionCertManager:
		// All cert-manager issuers need certManager enabled
		return fmt.Sprintf("{{- if .Values.certManager.enable }}\n%s{{- end }}", yamlContent)
	case kind == kindServiceMonitor && apiVersion == apiVersionMonitoring:
		// ServiceMonitors need prometheus enabled
		return fmt.Sprintf("{{- if .Values.prometheus.enable }}\n%s{{- end }}", yamlContent)
	case kind == kindServiceAccount || kind == kindRole || kind == kindClusterRole ||
		kind == kindRoleBinding || kind == kindClusterRoleBinding:
		// Distinguish between essential RBAC and helper RBAC
		if strings.Contains(name, "admin-role") || strings.Contains(name, "editor-role") ||
			strings.Contains(name, "viewer-role") {
			// Helper RBAC roles (admin/editor/viewer) - convenience roles for CRD management
			return fmt.Sprintf("{{- if .Values.rbacHelpers.enable }}\n%s{{- end }}\n", yamlContent)
		}
		if strings.Contains(name, "metrics") {
			// Metrics RBAC depends on metrics being enabled
			return fmt.Sprintf("{{- if .Values.metrics.enable }}\n%s{{- end }}\n", yamlContent)
		}
		// Essential RBAC (controller-manager, leader-election, manager roles) - always enabled
		// These are required for the controller to function properly
		return yamlContent
	case kind == kindValidatingWebhook || kind == kindMutatingWebhook:
		// Webhook configurations should always exist if project has webhooks
		// Only the cert-manager annotations should be conditional
		return t.makeWebhookAnnotationsConditional(yamlContent)
	case kind == kindService:
		// Services need conditional logic based on their purpose
		if strings.Contains(name, "metrics") {
			// Metrics services need metrics enabled
			return fmt.Sprintf("{{- if .Values.metrics.enable }}\n%s{{- end }}\n", yamlContent)
		}
		// Other services (webhook service, etc.) don't need conditionals - they're essential
		return yamlContent
	default:
		// No conditional wrapper needed for other resources (Deployment, Namespace)
		return yamlContent
	}
}

// collapseBlankLineAfterIf removes a single empty line that may appear
// immediately after a Helm if directive line, e.g. `{{- if ... }}`.
// This keeps templates compact and matches expected formatting in tests.
func (t *HelmTemplater) collapseBlankLineAfterIf(yamlContent string) string {
	lines := strings.Split(yamlContent, "\n")
	if len(lines) == 0 {
		return yamlContent
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// If current line is an if, and next line is blank, skip the blank
		if strings.Contains(line, "{{- if ") {
			out = append(out, line)
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				i++ // skip one blank line after if
			}
			continue
		}
		// If current line is blank, and next line is an end, skip the blank
		if strings.TrimSpace(line) == "" && i+1 < len(lines) && strings.Contains(lines[i+1], "{{- end }}") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
