package policyconditions

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	client "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nmstate "github.com/nmstate/kubernetes-nmstate/api/shared"
	nmstatev1beta1 "github.com/nmstate/kubernetes-nmstate/api/v1beta1"
	enactmentconditions "github.com/nmstate/kubernetes-nmstate/pkg/enactmentstatus/conditions"
	"github.com/nmstate/kubernetes-nmstate/pkg/node"
)

var (
	log = logf.Log.WithName("policyconditions")
)

func SetPolicyProgressing(conditions *nmstate.ConditionList, message string) {
	log.Info("SetPolicyProgressing")
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionDegraded,
		corev1.ConditionUnknown,
		nmstate.NodeNetworkConfigurationPolicyConditionConfigurationProgressing,
		"",
	)
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionAvailable,
		corev1.ConditionUnknown,
		nmstate.NodeNetworkConfigurationPolicyConditionConfigurationProgressing,
		message,
	)
}

func SetPolicySuccess(conditions *nmstate.ConditionList, message string) {
	log.Info("SetPolicySuccess")
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionDegraded,
		corev1.ConditionFalse,
		nmstate.NodeNetworkConfigurationPolicyConditionSuccessfullyConfigured,
		"",
	)
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionAvailable,
		corev1.ConditionTrue,
		nmstate.NodeNetworkConfigurationPolicyConditionSuccessfullyConfigured,
		message,
	)
}

func SetPolicyNotMatching(conditions *nmstate.ConditionList, message string) {
	log.Info("SetPolicyNotMatching")
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionDegraded,
		corev1.ConditionFalse,
		nmstate.NodeNetworkConfigurationPolicyConditionConfigurationNoMatchingNode,
		message,
	)
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionAvailable,
		corev1.ConditionTrue,
		nmstate.NodeNetworkConfigurationPolicyConditionConfigurationNoMatchingNode,
		message,
	)
}

func SetPolicyFailedToConfigure(conditions *nmstate.ConditionList, message string) {
	log.Info("SetPolicyFailedToConfigure")
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionDegraded,
		corev1.ConditionTrue,
		nmstate.NodeNetworkConfigurationPolicyConditionFailedToConfigure,
		message,
	)
	conditions.Set(
		nmstate.NodeNetworkConfigurationPolicyConditionAvailable,
		corev1.ConditionFalse,
		nmstate.NodeNetworkConfigurationPolicyConditionFailedToConfigure,
		"",
	)
}

func Update(cli client.Client, policyKey types.NamespacedName) error {
	logger := log.WithValues("policy", policyKey.Name)
	// On conflict we need to re-retrieve enactments since the
	// conflict can denote that the calculated policy conditions
	// are now not accurate.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		policy := &nmstatev1beta1.NodeNetworkConfigurationPolicy{}
		err := cli.Get(context.TODO(), policyKey, policy)
		if err != nil {
			return errors.Wrap(err, "getting policy failed")
		}

		enactments := nmstatev1beta1.NodeNetworkConfigurationEnactmentList{}
		policyLabelFilter := client.MatchingLabels{nmstate.EnactmentPolicyLabel: policy.Name}
		err = cli.List(context.TODO(), &enactments, policyLabelFilter)
		if err != nil {
			return errors.Wrap(err, "getting enactments failed")
		}

		// Count only nodes that runs nmstate handler, could be that
		// users don't want to run knmstate at master for example so
		// they don't want to change net config there.
		nmstateNodes, err := node.NodesRunningNmstate(cli)
		if err != nil {
			return errors.Wrap(err, "getting nodes running kubernets-nmstate pods failed")
		}
		numberOfNmstateNodes := len(nmstateNodes)

		// Let's get conditions with true status count filtered by policy generation
		enactmentsCount := enactmentconditions.Count(enactments, policy.Generation)

		numberOfFinishedEnactments := enactmentsCount.Available() + enactmentsCount.Failed() + enactmentsCount.NotMatching() + enactmentsCount.Aborted()

		logger.Info(fmt.Sprintf("enactments count: %s", enactmentsCount))
		if numberOfFinishedEnactments < numberOfNmstateNodes {
			SetPolicyProgressing(&policy.Status.Conditions, fmt.Sprintf("Policy is progressing %d/%d nodes finished", numberOfFinishedEnactments, numberOfNmstateNodes))
		} else {
			if enactmentsCount.Matching() == 0 {
				message := "Policy does not match any node"
				SetPolicyNotMatching(&policy.Status.Conditions, message)
			} else if enactmentsCount.Failed() > 0 || enactmentsCount.Aborted() > 0 {
				message := fmt.Sprintf("%d/%d nodes failed to configure", enactmentsCount.Failed(), enactmentsCount.Matching())
				if enactmentsCount.Aborted() > 0 {
					message += fmt.Sprintf(", %d nodes aborted configuration", enactmentsCount.Aborted())
				}
				SetPolicyFailedToConfigure(&policy.Status.Conditions, message)
			} else {
				message := fmt.Sprintf("%d/%d nodes successfully configured", enactmentsCount.Available(), enactmentsCount.Available())
				SetPolicySuccess(&policy.Status.Conditions, message)
			}
		}

		err = cli.Status().Update(context.TODO(), policy)
		if err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("conflict updating policy conditions, retrying")
			} else {
				logger.Error(err, "failed to update policy conditions")
			}
			return err
		}
		return nil
	})
}

func Reset(cli client.Client, policyKey types.NamespacedName) error {
	logger := log.WithValues("policy", policyKey.Name)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		policy := &nmstatev1beta1.NodeNetworkConfigurationPolicy{}
		err := cli.Get(context.TODO(), policyKey, policy)
		if err != nil {
			return errors.Wrap(err, "getting policy failed")
		}
		policy.Status.Conditions = nmstate.ConditionList{}
		err = cli.Status().Update(context.TODO(), policy)
		if err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("conflict resetting policy conditions, retrying")
			} else {
				logger.Error(err, "failed to reset policy conditions")
			}
			return err
		}
		return nil
	})
}
