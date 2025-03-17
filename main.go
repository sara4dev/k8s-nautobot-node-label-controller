package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// NautobotClient is a simple client to query Nautobot for device or rack info.
type NautobotClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

// NautobotDeviceData represents the minimal data we care about from Nautobot
type NautobotDeviceData struct {
	SiteName string
	RackName string
}

// Define the response structure to match the Nautobot API response
type deviceResponse struct {
	Results []struct {
		Site struct {
			Display string `json:"display"`
			Name    string `json:"name"`
		} `json:"site"`
		Rack struct {
			Display string `json:"display"`
			Name    string `json:"name"`
		} `json:"rack"`
	} `json:"results"`
}

// NewNautobotClient returns a new NautobotClient
func NewNautobotClient(baseURL, authToken string) *NautobotClient {
	return &NautobotClient{
		baseURL:    baseURL,
		authToken:  authToken,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetDeviceData queries Nautobot for a device's site and rack.
// In real usage, you'd likely query by a more reliable key, e.g., a device ID or an annotation.
func (c *NautobotClient) GetDeviceData(nodeName string) (*NautobotDeviceData, error) {
	// Extract the hostname part (before the first dot) to query Nautobot
	hostname := nodeName
	if dotIndex := strings.Index(nodeName, "."); dotIndex > 0 {
		hostname = nodeName[:dotIndex]
	}

	// Example: GET /api/dcim/devices/?name=<hostname>
	// This is an example endpoint — adjust to your actual Nautobot configuration/URL scheme.
	url := fmt.Sprintf("%s/api/dcim/devices/?name=%s", c.baseURL, hostname)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request to Nautobot: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to contact Nautobot: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Nautobot returned non-200 status: %d", resp.StatusCode)
	}

	var deviceResponse deviceResponse
	if err := json.NewDecoder(resp.Body).Decode(&deviceResponse); err != nil {
		return nil, fmt.Errorf("failed to parse Nautobot response: %w", err)
	}

	if len(deviceResponse.Results) == 0 {
		return nil, fmt.Errorf("no device found in Nautobot for node: %s", nodeName)
	}

	siteName := deviceResponse.Results[0].Site.Name
	// If name isn't available, fall back to display
	if siteName == "" {
		siteName = deviceResponse.Results[0].Site.Display
	}

	rackName := deviceResponse.Results[0].Rack.Name
	// If name isn't available, fall back to display
	if rackName == "" {
		rackName = deviceResponse.Results[0].Rack.Display
	}

	return &NautobotDeviceData{
		SiteName: siteName,
		RackName: rackName,
	}, nil
}

// NodeReconciler is our custom reconciler that will label Nodes with info from Nautobot.
type NodeReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NautobotClient *NautobotClient
}

// Reconcile is where we apply the logic to label the Node from Nautobot data.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Node", "NodeName", req.Name)

	// 1. Fetch the Node from Kubernetes
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		// If the Node is deleted or doesn't exist, just return
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the node already has our labels and they're non-empty
	// Skip reconciliation if the node already has all required labels
	if hasAllLabels(&node) {
		logger.Info("Node already has all required labels", "NodeName", node.Name)
		// Requeue after 12 hours for periodic refresh
		return ctrl.Result{RequeueAfter: 12 * time.Hour}, nil
	}

	// 2. Query Nautobot to get site and rack info
	deviceData, err := r.NautobotClient.GetDeviceData(node.Name)
	if err != nil {
		logger.Error(err, "Failed to get device data from Nautobot", "NodeName", node.Name)
		// Requeue with backoff for errors
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// 3. Update node labels if needed
	updated := false
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	// Only update if the value is different and the new value is not empty
	if deviceData.SiteName != "" && node.Labels["topology.kubernetes.io/zone"] != deviceData.SiteName {
		node.Labels["topology.kubernetes.io/zone"] = deviceData.SiteName
		updated = true
	}

	if deviceData.RackName != "" && node.Labels["topology.kubernetes.io/rack"] != deviceData.RackName {
		node.Labels["topology.kubernetes.io/rack"] = deviceData.RackName
		updated = true
	}

	// 4. Persist changes if the labels changed
	if updated {
		logger.Info("Updating node labels", "NodeName", node.Name, "Site", deviceData.SiteName, "Rack", deviceData.RackName)
		if err := r.Update(ctx, &node); err != nil {
			logger.Error(err, "Failed to update node labels")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
	}

	// If we got here, no updates were needed
	logger.Info("No label updates needed", "NodeName", node.Name)
	return ctrl.Result{RequeueAfter: 6 * time.Hour}, nil
}

// hasAllLabels checks if the node already has all the required labels with non-empty values
func hasAllLabels(node *corev1.Node) bool {
	if node.Labels == nil {
		return false
	}

	zone, hasZone := node.Labels["topology.kubernetes.io/zone"]
	rack, hasRack := node.Labels["topology.kubernetes.io/rack"]

	return hasZone && hasRack && zone != "" && rack != ""
}

// SetupWithManager registers the controller with the manager
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}). // Watch Node objects
		Complete(r)
}

// main sets up the manager and starts the controller
func main() {
	// Set up logging
	opts := zap.Options{
		Development: true,
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Grab environment variables or flags for config
	nautobotURL := os.Getenv("NAUTOBOT_URL")
	if nautobotURL == "" {
		nautobotURL = "http://nautobot.local" // fallback or placeholder
	}
	nautobotToken := os.Getenv("NAUTOBOT_TOKEN")
	if nautobotToken == "" {
		nautobotToken = "placeholder-token"
	}

	// Create a controller-runtime manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: runtime.NewScheme(),
		// You can fine-tune the cache if you want to limit which objects you watch
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				metav1.NamespaceAll: {},
			},
		},
		// Leader election, metrics, etc. can be configured here
	})
	if err != nil {
		panic(fmt.Sprintf("Unable to create manager: %v", err))
	}

	// Add core types (Node, etc.) to the scheme
	if err := corev1.AddToScheme(mgr.GetScheme()); err != nil {
		panic(fmt.Sprintf("Unable to add corev1 to scheme: %v", err))
	}

	// Create the Nautobot client
	nautobotClient := NewNautobotClient(nautobotURL, nautobotToken)

	// Create and register our Reconciler
	reconciler := &NodeReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		NautobotClient: nautobotClient,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("Unable to setup NodeReconciler with manager: %v", err))
	}

	// Start the manager (blocking call)
	fmt.Println("Starting Nautobot Node Labeler Controller...")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		panic(fmt.Sprintf("Manager exited non-zero: %v", err))
	}
}
