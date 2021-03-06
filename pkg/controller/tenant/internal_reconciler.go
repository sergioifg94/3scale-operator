package tenant

import (
	"bytes"
	"context"
	"fmt"
	"reflect"

	apiv1alpha1 "github.com/3scale/3scale-operator/pkg/apis/capabilities/v1alpha1"
	porta_client_pkg "github.com/3scale/3scale-porta-go-client/client"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// InternalReconciler reconciles a Tenant object
type InternalReconciler struct {
	k8sClient   client.Client
	tenantR     *apiv1alpha1.Tenant
	portaClient *porta_client_pkg.ThreeScaleClient
	logger      logr.Logger
}

// NewInternalReconciler constructs InternalReconciler object
func NewInternalReconciler(k8sClient client.Client, tenantR *apiv1alpha1.Tenant,
	portaClient *porta_client_pkg.ThreeScaleClient, log logr.Logger) *InternalReconciler {
	return &InternalReconciler{
		k8sClient:   k8sClient,
		tenantR:     tenantR,
		portaClient: portaClient,
		logger:      log,
	}
}

// Run tenant reconciliation logic
// Facts to reconcile:
// - Have 3scale Tenant Account
// - Have active admin user
// - Have secret with tenant's access_token
func (r *InternalReconciler) Run() error {
	tenantDef, err := r.reconcileTenant()
	if err != nil {
		return err
	}

	adminUserDef, err := r.reconcileAdminUser(tenantDef)
	if err != nil {
		return err
	}

	err = r.reconcileAccessTokenSecret(tenantDef)
	if err != nil {
		return err
	}

	tenantStatus := r.getTenantStatus(tenantDef, adminUserDef)

	return r.updateTenantStatus(tenantStatus)
}

// This method makes sure that tenant exists, otherwise it will create one
// On method completion:
// * tenant will exist
// * tenant's attributes will be updated if required
func (r *InternalReconciler) reconcileTenant() (*porta_client_pkg.Tenant, error) {
	tenantDef, err := r.fetchTenant()
	if err != nil {
		return nil, err
	}

	if tenantDef == nil {
		tenantDef, err = r.createTenant()
		if err != nil {
			return nil, err
		}
	} else {
		r.logger.Info("Tenant already exists", "TenantId", tenantDef.Signup.Account.ID)
		// Tenant is not created, check tenant desired state matches current state
		// When created, not needed to update
		err := r.syncTenant(tenantDef)
		if err != nil {
			return nil, err
		}
	}

	return tenantDef, nil
}

func (r *InternalReconciler) fetchTenant() (*porta_client_pkg.Tenant, error) {
	if r.tenantR.Status.TenantId == 0 {
		// tenantId not in status field
		// Tenant has to be created
		return nil, nil
	}

	tenantDef, err := r.portaClient.ShowTenant(r.tenantR.Status.TenantId)
	if err != nil && porta_client_pkg.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return tenantDef, nil
}

func (r *InternalReconciler) syncTenant(tenantDef *porta_client_pkg.Tenant) error {
	// If tenant desired state is not current state, update
	triggerSync := func() bool {
		if r.tenantR.Spec.OrganizationName != tenantDef.Signup.Account.OrgName {
			return true
		}

		if r.tenantR.Spec.Email != tenantDef.Signup.Account.SupportEmail {
			return true
		}

		return false
	}()

	if triggerSync {
		r.logger.Info("Syncing tenant", "TenantId", tenantDef.Signup.Account.ID)
		tenantDef.Signup.Account.OrgName = r.tenantR.Spec.OrganizationName
		tenantDef.Signup.Account.SupportEmail = r.tenantR.Spec.Email
		params := porta_client_pkg.Params{
			"support_email": r.tenantR.Spec.Email,
			"org_name":      r.tenantR.Spec.OrganizationName,
		}
		_, err := r.portaClient.UpdateTenant(r.tenantR.Status.TenantId, params)
		if err != nil {
			return err
		}
	}

	return nil
}

////
//
// This method makes sure admin user:
// * is active
// * user's attributes will be updated if required
func (r *InternalReconciler) reconcileAdminUser(tenantDef *porta_client_pkg.Tenant) (*porta_client_pkg.User, error) {
	adminUserDef, err := r.fetchAdminUser(tenantDef)
	if err != nil {
		return nil, err
	}

	err = r.syncAdminUser(tenantDef, adminUserDef)
	if err != nil {
		return nil, err
	}

	return adminUserDef, nil
}

// This method makes sure secret with tenant's access_token exists
func (r *InternalReconciler) reconcileAccessTokenSecret(tenantDef *porta_client_pkg.Tenant) error {
	tenantProviderKeySecretNN := types.NamespacedName{
		Name:      r.tenantR.Spec.TenantSecretRef.Name,
		Namespace: r.tenantR.Spec.TenantSecretRef.Namespace,
	}
	tenantProviderKeySecret, err := r.findAccessTokenSecret(tenantProviderKeySecretNN)
	if err != nil {
		return err
	}

	if tenantProviderKeySecret == nil {
		err = r.createTenantProviderKeySecret(tenantDef, tenantProviderKeySecretNN)
		if err != nil {
			return err
		}
	} else {
		r.logger.Info("Admin user access token secret already exists",
			"Secret NS", tenantProviderKeySecretNN.Namespace, "Secret name", tenantProviderKeySecretNN.Name)
	}
	return nil
}

// Create Tenant using porta client
func (r *InternalReconciler) createTenant() (*porta_client_pkg.Tenant, error) {
	password, err := r.getAdminPassword()
	if err != nil {
		return nil, err
	}

	r.logger.Info("Creating a new tenant", "OrganizationName", r.tenantR.Spec.OrganizationName,
		"Username", r.tenantR.Spec.Username, "Email", r.tenantR.Spec.Email)
	return r.portaClient.CreateTenant(
		r.tenantR.Spec.OrganizationName,
		r.tenantR.Spec.Username,
		r.tenantR.Spec.Email,
		password,
	)
}

func (r *InternalReconciler) getAdminPassword() (string, error) {
	// Get tenant admin password from secret reference
	tenantAdminSecret := &v1.Secret{}

	err := r.k8sClient.Get(context.TODO(),
		types.NamespacedName{
			Name:      r.tenantR.Spec.PasswordCredentialsRef.Name,
			Namespace: r.tenantR.Namespace,
		},
		tenantAdminSecret)

	if err != nil {
		return "", err
	}

	passwordByteArray, ok := tenantAdminSecret.Data[TenantAdminPasswordSecretField]
	if !ok {
		return "", fmt.Errorf("Not found admin password secret (ns: %s, name: %s) attribute: %s",
			r.tenantR.Namespace, r.tenantR.Spec.PasswordCredentialsRef.Name,
			TenantAdminPasswordSecretField)
	}

	return bytes.NewBuffer(passwordByteArray).String(), err
}

//
func (r *InternalReconciler) fetchAdminUser(tenantDef *porta_client_pkg.Tenant) (*porta_client_pkg.User, error) {
	if r.tenantR.Status.AdminId == 0 {
		// UserID not in status field
		return r.findAdminUser(tenantDef)
	}

	//
	return r.portaClient.ReadUser(tenantDef.Signup.Account.ID, r.tenantR.Status.AdminId)
}

func (r *InternalReconciler) findAdminUser(tenantDef *porta_client_pkg.Tenant) (*porta_client_pkg.User, error) {
	// Only admin users
	// Any state
	filterParams := porta_client_pkg.Params{
		"role": "admin",
	}
	userList, err := r.portaClient.ListUsers(tenantDef.Signup.Account.ID, filterParams)
	if err != nil {
		return nil, err
	}

	for _, user := range userList.Users {
		if user.User.Email == r.tenantR.Spec.Email && user.User.UserName == r.tenantR.Spec.Username {
			// user is already a copy from User slice element
			return &user.User, nil
		}
	}
	return nil, fmt.Errorf("Admin user not found and should be available"+
		"TenantId: %d. Admin Username: %s, Admin email: %s", tenantDef.Signup.Account.ID,
		r.tenantR.Spec.Username, r.tenantR.Spec.Email)
}
func (r *InternalReconciler) syncAdminUser(tenantDef *porta_client_pkg.Tenant, adminUser *porta_client_pkg.User) error {
	// If adminUser desired state is not current state, update
	if adminUser.State == "pending" {
		err := r.activateAdminUser(tenantDef, adminUser)
		if err != nil {
			return err
		}
	} else {
		r.logger.Info("Admin user already active", "TenantId", tenantDef.Signup.Account.ID, "UserID", adminUser.ID)
	}

	triggerSync := func() bool {
		if r.tenantR.Spec.Username != adminUser.UserName {
			return true
		}

		if r.tenantR.Spec.Email != adminUser.Email {
			return true
		}

		return false
	}()

	if triggerSync {
		r.logger.Info("Syncing adminUser", "TenantId", tenantDef.Signup.Account.ID, "UserID", adminUser.ID)
		adminUser.UserName = r.tenantR.Spec.Username
		adminUser.Email = r.tenantR.Spec.Email
		params := porta_client_pkg.Params{
			"username": r.tenantR.Spec.Username,
			"email":    r.tenantR.Spec.Email,
		}
		_, err := r.portaClient.UpdateUser(tenantDef.Signup.Account.ID, adminUser.ID, params)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *InternalReconciler) activateAdminUser(tenantDef *porta_client_pkg.Tenant, adminUser *porta_client_pkg.User) error {
	r.logger.Info("Activating pending admin user", "Account ID", tenantDef.Signup.Account.ID, "ID", adminUser.ID)
	return r.portaClient.ActivateUser(tenantDef.Signup.Account.ID, adminUser.ID)
}

func (r *InternalReconciler) findAccessTokenSecret(nn types.NamespacedName) (*v1.Secret, error) {
	adminAccessTokenSecret := &v1.Secret{}

	err := r.k8sClient.Get(context.TODO(), nn, adminAccessTokenSecret)

	if err != nil && errors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return adminAccessTokenSecret, nil
}

func (r *InternalReconciler) createTenantProviderKeySecret(tenantDef *porta_client_pkg.Tenant, nn types.NamespacedName) error {
	r.logger.Info("Creating admin access token secret", "Secret NS", nn.Namespace, "Secret name", nn.Name)

	tenantProviderKey, err := r.findTenantProviderKey(tenantDef)
	if err != nil {
		return err
	}

	adminURL, err := URLFromDomain(tenantDef.Signup.Account.AdminDomain)
	if err != nil {
		return err
	}

	secret := &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: nn.Namespace,
			Name:      nn.Name,
			Labels:    map[string]string{"app": "3scale-operator"},
		},
		StringData: map[string]string{
			TenantProviderKeySecretField:    tenantProviderKey,
			TenantAdminDomainKeySecretField: adminURL.String(),
		},
		Type: v1.SecretTypeOpaque,
	}
	addOwnerRefToObject(secret, asOwner(r.tenantR))
	return r.k8sClient.Create(context.TODO(), secret)
}

func (r *InternalReconciler) findTenantProviderKey(tenantDef *porta_client_pkg.Tenant) (string, error) {
	// Tenant Provider Key is available on provider application list
	appList, err := r.portaClient.ListApplications(tenantDef.Signup.Account.ID)
	if err != nil {
		return "", err
	}

	if len(appList.Applications) != 1 {
		return "", fmt.Errorf("Unexpected application list. TenantId: %d", tenantDef.Signup.Account.ID)
	}

	return appList.Applications[0].Application.UserKey, nil
}

func (r *InternalReconciler) getTenantStatus(tenantDef *porta_client_pkg.Tenant, adminUserDef *porta_client_pkg.User) *apiv1alpha1.TenantStatus {
	return &apiv1alpha1.TenantStatus{
		TenantId: tenantDef.Signup.Account.ID,
		AdminId:  adminUserDef.ID,
	}
}

func (r *InternalReconciler) updateTenantStatus(tenantStatus *apiv1alpha1.TenantStatus) error {
	// don't update the status if there aren't any changes.
	if reflect.DeepEqual(r.tenantR.Status, *tenantStatus) {
		return nil
	}
	r.logger.Info("update tenant status", "status", tenantStatus)
	r.tenantR.Status = *tenantStatus
	return r.k8sClient.Status().Update(context.TODO(), r.tenantR)
}
