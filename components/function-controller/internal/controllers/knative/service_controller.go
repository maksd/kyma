package knative

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	apilabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/kyma/components/function-controller/internal/resource"
	serverlessv1alpha1 "github.com/kyma-project/kyma/components/function-controller/pkg/apis/serverless/v1alpha1"
)

const (
	serviceLabelKey    = "serving.knative.dev/service"
	cfgGenerationLabel = "serving.knative.dev/configurationGeneration"
)

type ServiceConfig struct {
	RequeueDuration time.Duration `envconfig:"default=10m"`
}

type ServiceReconciler struct {
	Log logr.Logger

	config ServiceConfig
	client resource.Client
	scheme *runtime.Scheme
}

func NewServiceReconciler(client resource.Client, log logr.Logger, cfg ServiceConfig) *ServiceReconciler {
	return &ServiceReconciler{
		Log:    log.WithName("controllers").WithName("kservice"),
		config: cfg,
		client: client,
	}
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kservice-controller").
		For(&servingv1.Service{}).
		Owns(&servingv1.Revision{}).
		WithEventFilter(r.getPredicates()).
		Complete(r)
}

// Reconcile reads that state of the cluster for a Function object and makes changes based on the state read and what is in the Function.Spec
// +kubebuilder:rbac:groups="serving.knative.dev",resources=revisions,verbs=get;list;watch;deletecollection
// +kubebuilder:rbac:groups="serving.knative.dev",resources=services;revisions,verbs=get
// +kubebuilder:rbac:groups="serving.knative.dev",resources=services/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ServiceReconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	instance := &servingv1.Service{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		r.Log.WithValues("service", request.NamespacedName).Error(err, "unable to fetch Service")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hasCorrectLabels(*instance) {
		r.Log.WithValues("service", request.NamespacedName).Info("skipping reconcilation for a service without needed labels")
		return ctrl.Result{}, nil
	}

	if !instance.Status.IsReady() {
		return ctrl.Result{}, nil
	}

	log := r.Log.WithValues("kind", instance.GetObjectKind().GroupVersionKind().Kind, "name", instance.GetName(), "namespace", instance.GetNamespace(), "version", instance.GetGeneration())

	log.Info("Listing Revisions")
	var revisions servingv1.RevisionList
	if err := r.client.ListByLabel(ctx, instance.GetNamespace(), r.serviceLabel(instance), &revisions); err != nil {
		log.Error(err, "Cannot list Revisions")
		return ctrl.Result{}, err
	}

	if err := r.deleteRevisions(ctx, log, instance, revisions.Items); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.config.RequeueDuration}, nil
}

func hasCorrectLabels(instance servingv1.Service) bool {
	labels := instance.GetLabels()

	_, managedByLabelOk := labels[serverlessv1alpha1.FunctionManagedByLabel]
	_, nameLabelOk := labels[serverlessv1alpha1.FunctionNameLabel]
	_, uuidLabel := labels[serverlessv1alpha1.FunctionUUIDLabel]

	return managedByLabelOk && nameLabelOk && uuidLabel
}

func (r *ServiceReconciler) deleteRevisions(ctx context.Context, log logr.Logger, service *servingv1.Service, revisions []servingv1.Revision) error {
	log.Info("Deleting all old revisions")
	selector, err := r.getOldRevisionSelector(service.Name, revisions)
	if err != nil {
		log.Error(err, "Cannot build proper selector for old revisions")
		return err
	}

	if err := r.client.DeleteAllBySelector(ctx, &servingv1.Revision{}, service.GetNamespace(), selector); err != nil {
		log.Error(err, "Cannot delete old Revisions")
		return err
	}
	log.Info("Old Revisions deleted")
	return nil
}

func (r *ServiceReconciler) serviceLabel(s *servingv1.Service) map[string]string {
	return map[string]string{
		serviceLabelKey: s.Name,
	}
}

func (r *ServiceReconciler) getOldRevisionSelector(parentService string, revisions []servingv1.Revision) (apilabels.Selector, error) {
	maxGen, err := getNewestGeneration(revisions)
	if err != nil {
		return nil, err
	}

	selector := apilabels.NewSelector()
	uuidReq, err := apilabels.NewRequirement(serviceLabelKey, selection.Equals, []string{parentService})
	if err != nil {
		return nil, err
	}
	generationReq, err := apilabels.NewRequirement(cfgGenerationLabel, selection.NotEquals, []string{strconv.Itoa(maxGen)})
	if err != nil {
		return nil, err
	}

	return selector.Add(*uuidReq, *generationReq), nil
}

func getNewestGeneration(revisions []servingv1.Revision) (int, error) {
	maxGeneration := -1
	for _, revision := range revisions {
		generationString, ok := revision.Labels[cfgGenerationLabel]
		if !ok {
			// todo extract to var
			return -1, fmt.Errorf("revision %s in namespace %s doesn't have %s label", revision.Name, revision.Namespace, cfgGenerationLabel)
		}
		generation, err := strconv.Atoi(generationString)
		if err != nil {
			// todo extract to var
			return -1, fmt.Errorf("couldn't convert label key %s to number, revision %s in namespace %s", generationString, revision.Name, revision.Namespace)
		}
		if generation > maxGeneration {
			maxGeneration = generation
		}
	}
	return maxGeneration, nil
}
