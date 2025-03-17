package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	// Example: GET /api/dcim/devices/?name=<nodeName>
	// This is an example endpoint — adjust to your actual Nautobot configuration/URL scheme.
	url := fmt.Sprintf("%s/api/dcim/devices/?name=%s", c.baseURL, nodeName)

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

	// 1. Fetch the Node from Kubernetes
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		// If the Node is deleted or doesn't exist, just return
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling Node", "NodeName", node.Name)

	// 2. Query Nautobot to get site and rack info
	deviceData, err := r.NautobotClient.GetDeviceData(node.Name)
	if err != nil {
		logger.Error(err, "Failed to get device data from Nautobot", "NodeName", node.Name)
		// You may choose to requeue with some backoff or just return an error
		return ctrl.Result{}, err
	}

	// 3. Update node labels if needed
	updated := false
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	// Example: label the node "topology.kubernetes.io/zone" with the site name
	// and "topology.kubernetes.io/rack" with the rack name
	if node.Labels["topology.kubernetes.io/zone"] != deviceData.SiteName {
		node.Labels["topology.kubernetes.io/zone"] = deviceData.SiteName
		updated = true
	}
	if node.Labels["topology.kubernetes.io/rack"] != deviceData.RackName {
		node.Labels["topology.kubernetes.io/rack"] = deviceData.RackName
		updated = true
	}

	// 4. Persist changes if the labels changed
	if updated {
		logger.Info("Updating node labels", "NodeName", node.Name, "Site", deviceData.SiteName, "Rack", deviceData.RackName)
		if err := r.Update(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update node labels: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}). // Watch Node objects
		Complete(r)
}

// main sets up the manager and starts the controller
func main() {
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
