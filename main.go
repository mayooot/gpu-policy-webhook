package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"net/http"
	"strings"

	"k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	port        = flag.Int("port", 8443, "Webhook server port")
	certFile    = flag.String("tls-cert", "/etc/webhook/certs/tls.crt", "TLS certificate file")
	keyFile     = flag.String("tls-key", "/etc/webhook/certs/tls.key", "TLS key file")
	gpuPrefixes = flag.String("gpu-prefixes", "nvidia.com", "Comma-separated GPU resource prefixes (e.g., nvidia.com,amd.com)")
	kubeconfig  = flag.String("kubeconfig", "", "Path to a kubeconfig. If not specified will use default path, then in-cluster config")
)

type WebhookServer struct {
	scheme  *runtime.Scheme
	decoder *serializer.CodecFactory

	gpuPrefixes []string
	kubeconfig  string
	clientset   *kubernetes.Clientset
}

func NewWebhookServer() *WebhookServer {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	codecFactory := serializer.NewCodecFactory(scheme)
	return &WebhookServer{
		scheme:  scheme,
		decoder: &codecFactory,
	}
}

func main() {
	flag.Parse()

	server := NewWebhookServer()
	server.gpuPrefixes = strings.Split(*gpuPrefixes, ",")
	server.kubeconfig = *kubeconfig
	server.initClientsetOrDie()

	http.HandleFunc("/validate", server.validatePod)

	// Set up TLS
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	srv := &http.Server{
		Addr:      fmt.Sprintf(":%d", *port),
		TLSConfig: tlsConfig,
	}

	klog.Infof("Starting webhook server on port %d with GPU prefixes: %v", *port, *gpuPrefixes)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		klog.Fatalf("Failed to start server: %v", err)
	}
}

func (s *WebhookServer) validatePod(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// Decode AdmissionReview request
	ar := v1.AdmissionReview{}
	deserializer := s.decoder.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		http.Error(w, fmt.Sprintf("failed to decode body: %v", err), http.StatusBadRequest)
		return
	}

	// Process Pod
	pod := corev1.Pod{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		http.Error(w, fmt.Sprintf("failed to unmarshal pod: %v", err), http.StatusBadRequest)
		return
	}

	// Validate GPU resources
	response := s.validateGPUResources(&pod, ar.Request.Namespace)
	response.UID = ar.Request.UID

	// Send response
	respBytes, err := json.Marshal(v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: response,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to marshal response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

func (s *WebhookServer) validateGPUResources(pod *corev1.Pod, namespace string) *v1.AdmissionResponse {
	response := &v1.AdmissionResponse{
		Allowed: true,
	}

	// Check each container's resource requirements
	for _, container := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		for resourceName, _ := range container.Resources.Requests {
			for _, prefix := range s.gpuPrefixes {
				if strings.HasPrefix(string(resourceName), prefix) {
					response.Allowed = false
					response.Result = &metav1.Status{
						Message: fmt.Sprintf("GPU resource %s is not allowed in namespace %s", resourceName, namespace),
						Reason:  metav1.StatusReasonForbidden,
					}
					return response
				}
			}
		}
	}
	return response
}

func (s *WebhookServer) initClientsetOrDie() {
	config, err := clientcmd.BuildConfigFromFlags("", s.kubeconfig)
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	s.clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}
	klog.Infof("Successfully initialized kubernetes clientset")
}
