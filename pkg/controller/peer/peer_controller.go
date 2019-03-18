/*
Copyright 2019 The kubewg Authors.

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

package peer

import (
	"context"
	"net"
	"reflect"
	"strings"

	wgv1alpha1 "github.com/munnerz/kubewg/pkg/apis/wg/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller")

// Add creates a new Peer Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcilePeer{Client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("peer-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Peer
	err = c.Watch(&source.Kind{Type: &wgv1alpha1.Peer{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO: use a custom mapper to only trigger resyncs of the appropriate Peers
	err = c.Watch(&source.Kind{Type: &wgv1alpha1.RouteBinding{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to Network
	err = c.Watch(&source.Kind{Type: &wgv1alpha1.Network{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcilePeer{}

// ReconcilePeer reconciles a Peer object
type ReconcilePeer struct {
	client.Client
	scheme *runtime.Scheme
}

func (r *ReconcilePeer) computePeerConfigurations(subnet, addr string, p *wgv1alpha1.Peer) ([]wgv1alpha1.PeerConfiguration, error) {
	log.Info("Finding peers within network")
	var peers wgv1alpha1.PeerList
	if err := r.Client.List(context.TODO(), &client.ListOptions{Namespace: p.Namespace}, &peers); err != nil {
		return nil, err
	}

	var cfgs []wgv1alpha1.PeerConfiguration
	for _, peer := range peers.Items {
		if p.Status.Network != peer.Status.Network {
			log.Info("Skipping peer as it is not within the same network", "peer", peer.Name)
			continue
		}
		if peer.Status.Address == "" {
			log.Info("Skipping adding peer to allowedIPs as it has no address", "peer", peer.Name)
			continue
		}
		if peer.Name == p.Name {
			log.Info("Skipping self", "peer", peer.Name)
			continue
		}
		log.Info("Creating new Peer entry", "peer", peer.Name)
		cfg := wgv1alpha1.PeerConfiguration{
			Name:      peer.Name,
			PublicKey: peer.Spec.PublicKey,
		}
		host, _, err := net.SplitHostPort(peer.Spec.Endpoint)
		if err != nil {
			return nil, err
		}
		if host != "" {
			log.Info("Setting peer endpoint", "peer", peer.Name, "endpoint", peer.Spec.Endpoint)
			cfg.Endpoint = peer.Spec.Endpoint
		}
		// add each peers IP address as an AllowedIP
		peerIP, _ := splitNetMask(peer.Status.Address)
		cfg.AllowedIPs = []string{peerIP + "/32"}
		// add RouteRule configurations
		routeRules, err := r.routeRulesForPeer(&peer)
		if err != nil {
			return nil, err
		}
		for _, rr := range routeRules {
			cfg.AllowedIPs = append(cfg.AllowedIPs, rr.Spec.Routes...)
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, nil
}

func splitNetMask(s string) (string, string) {
	idx := strings.Index(s, "/")
	if idx == -1 || idx == len(s)-1 {
		// TODO: support IPv6
		return s, "32"
	}
	return s[:idx], s[idx+1:]
}

func (r *ReconcilePeer) routeRulesForPeer(p *wgv1alpha1.Peer) ([]wgv1alpha1.RouteBinding, error) {
	var routeRules wgv1alpha1.RouteBindingList
	if err := r.Client.List(context.TODO(), &client.ListOptions{Namespace: p.Namespace}, &routeRules); err != nil {
		return nil, err
	}
	var bindings []wgv1alpha1.RouteBinding
	for _, rr := range routeRules.Items {
		if !peerMatchesSelector(p, rr.Spec.Selector) {
			continue
		}

		bindings = append(bindings, rr)
	}
	return bindings, nil
}

func peerMatchesSelector(p *wgv1alpha1.Peer, sel wgv1alpha1.PeerSelector) bool {
	for _, n := range sel.Names {
		if p.Name == n {
			return true
		}
	}
	if len(sel.MatchLabels) == 0 {
		return false
	}
	found := true
	for k, v := range sel.MatchLabels {
		if v2, ok := p.Labels[k]; !ok || v != v2 {
			found = false
			break
		}
	}
	if found {
		return true
	}
	return false
}

// Reconcile reads that state of the cluster for a Peer object and makes changes based on the state read
// and what is in the Peer.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  The scaffolding writes
// a Deployment as an example
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
// +kubebuilder:rbac:groups=wg.mnrz.xyz,resources=networks;routebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=wg.mnrz.xyz,resources=peers,verbs=get;list;watch
// +kubebuilder:rbac:groups=wg.mnrz.xyz,resources=peers/status,verbs=update;patch
func (r *ReconcilePeer) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the Peer instance
	instance := &wgv1alpha1.Peer{}
	err := r.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	log.Info("Finding network for peer", "network", instance.Status.Network)
	network := &wgv1alpha1.Network{}
	err = r.Get(context.TODO(), client.ObjectKey{Namespace: request.Namespace, Name: instance.Status.Network}, network)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Network not found", "network", instance.Status.Network)
			return reconcile.Result{}, nil
		}
		log.Error(err, "Error looking up Network resource", "network", instance.Status.Network)
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	log.Info("Computing peer configuration")
	peer := instance.DeepCopy()
	// TODO: validate peer.spec.address is a valid address in network.spec.subnet
	cfgs, err := r.computePeerConfigurations(network.Spec.Subnet, peer.Status.Address, peer)
	if err != nil {
		return reconcile.Result{}, err
	}

	peer.Status.Peers = cfgs

	if !reflect.DeepEqual(instance.Status, peer.Status) {
		log.Info("Updating Peer", "namespace", peer.Namespace, "name", peer.Name)
		err = r.Update(context.TODO(), peer)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}