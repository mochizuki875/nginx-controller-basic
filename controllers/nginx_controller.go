/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"strconv"

	nginxv1 "example.com/nginx-controller/api/v1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	deploymentOwnerKey = ".metadata.controller"
	apiGVStr           = nginxv1.GroupVersion.String()
)

// NginxReconciler reconciles a Nginx object
type NginxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// OwnerReferenceに設定されたNginxリソースの名前に対応しないDeploymentを削除する
// Nginxリソースによって作られるDeployment名はdeploy-<Nginxリソース名>になる
func (r *NginxReconciler) cleanupOwnerResources(ctx context.Context, log logr.Logger, nginx *nginxv1.Nginx, deploymentName string) error {
	// log.Info("Finding existing Deployments for Nginx resource")

	/* 以下の条件でDeploymentのListを取得
	   ・Nginxリソースと同じNamespace
	   ・".metadata.controller": <Nginxリソース名>というIndexが付与されている
	*/
	var deploymentList appsv1.DeploymentList
	if err := r.List(ctx, &deploymentList, client.InNamespace(nginx.Namespace), client.MatchingFields(map[string]string{deploymentOwnerKey: nginx.Name})); err != nil {
		return err
	}

	for _, deployment := range deploymentList.Items {
		// 取得したDeployment名とNginxで作成されたDeployment名(deploymentName)を比較
		if deployment.Name == deploymentName { // trueならDeploymentがNginxに管理されていることになるので削除しない
			// 比較した結果が一致したら何もしない
			continue // 処理をスキップ
		}

		// 比較した結果差分があればDeploymentを削除
		if err := r.Delete(ctx, &deployment); err != nil { // 上でfalseの場合はDeploymentを削除
			log.Error(err, "Faild to delete Deployment")
			return err
		}
		log.Info("Delete Deployment resource: " + deployment.Name)

	}
	return nil
}

// Nginxリソースに対応したDeploymentを作成/更新
func (r *NginxReconciler) CreateOrUpdateDeployment(ctx context.Context, log logr.Logger, nginx *nginxv1.Nginx, deploymentName string) error {

	log.Info("CreateOrUpdate Deployment for " + nginx.Name)

	var operationResult controllerutil.OperationResult
	// Deploymentを作成(structの初期化)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: nginx.Namespace,
		},
	}

	// コールバック関数の中でDeployment(deploy)を定義しCreateOrUpdateで作成/更新
	/*
		r.GET()やr.List()は
		controller-runtime/client.Client
		https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/client#Client
		で定義されたメソッドであるのに対し

		ctrl.CreateOrUpdateは
		controller-runtime
		https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0#pkg-variables
		のvarとして定義されたcontrollerutilで実装されたメソッド
		https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/controller/controllerutil
		https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/controller/controllerutil#CreateOrUpdate
	*/
	operationResult, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		// コールバック関数funcの中でDeploymentの作成を実施
		// この関数の中で作成したオブジェクトをもとに差分比較を行うらしい
		// https://github.com/kubernetes-sigs/controller-runtime/blob/d242fe21e646f034995c4c93e9bba388a0fdaab9/pkg/controller/controllerutil/controllerutil.go#L210-L217

		replicas := int32(1) // 初期値
		if nginx.Spec.Replicas != nil {
			replicas = *nginx.Spec.Replicas // Nginx ObjectのSpecからReplicasを取得
		}
		deploy.Spec.Replicas = &replicas // DeploymentにReplicasを設定

		// LabelをMapで定義
		labels := map[string]string{
			"controller": nginx.Name,
		}

		// DeploymentのLabelSelectorにlabelsを設定
		// https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#LabelSelector
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}

		// Pod Templateにlabelsを設定
		// https://pkg.go.dev/k8s.io/api@v0.25.0/core/v1#PodTemplateSpec
		if deploy.Spec.Template.Labels == nil {
			deploy.Spec.Template.Labels = labels
		}

		// Containerをarrayで定義
		// https://pkg.go.dev/k8s.io/api@v0.25.0/core/v1#Container
		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}

		// Pod TemplateにContainerを設定
		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		// ★DeploymentにOwnerReferenceを設定
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil#SetControllerReference
		if err := ctrl.SetControllerReference(nginx, deploy, r.Scheme); err != nil {
			log.Error(err, "Unable to set OwnerReference from Nginx to Deployment")
		}

		return nil

	})
	if err != nil {
		log.Error(err, "Unable to ensure deployment is correct")
		return err
	}

	log.Info("CreateOrUpdate Deployment for " + nginx.Name + ": " + string(operationResult))

	return nil

}

//+kubebuilder:rbac:groups=nginx.my.domain,resources=nginxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nginx.my.domain,resources=nginxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=nginx.my.domain,resources=nginxes/finalizers,verbs=update

// reconcile.Reconcileインターフェイスを実装
// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *NginxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	defer fmt.Println("=== End Reconcile " + "for " + req.Name + " ===")
	fmt.Println("=== Start Reconcile " + "for " + req.Name + " ===")

	log := log.FromContext(ctx) // contextに含まれるvalueを付与してログを出力するlogger

	var nginx nginxv1.Nginx
	var deployment appsv1.Deployment
	var err error

	// ①cacheから変更のあったNginx Objectを取得する
	if err = r.Get(ctx, req.NamespacedName, &nginx); err != nil {
		log.Error(err, "Unable to fetch Nginx from cache.")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	deploymentName := "deploy-" + nginx.Name // Nginxにより管理されるDeploymentの名前

	// ②Nginxが過去に管理していたDeploymentを削除する
	if err = r.cleanupOwnerResources(ctx, log, &nginx, deploymentName); err != nil {
		log.Error(err, "Faild to clean up old Deployment")
		return ctrl.Result{}, err
	}

	// ③Nginxが管理するDeploymentを作成/更新する
	if err = r.CreateOrUpdateDeployment(ctx, log, &nginx, deploymentName); err != nil {
		return ctrl.Result{}, err
	}

	// ④Nginx ObjectのStatusを更新する
	// controller-runtimeのclientで定義されているObjectKey型でDeploymentのNamespacedNameを設定
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/client#ObjectKey
	// ※NamespacedName型が定義できれば良いので以下を直接定義しても多分OK(controller-runtimeがapimachineryをラップしている例)
	// https://pkg.go.dev/k8s.io/apimachinery/pkg/types#NamespacedName

	availableReplicasFlag := false
	deploymentNameFlag := false

	deploymentNamespacedName := client.ObjectKey{
		Namespace: req.Namespace,
		Name:      deploymentName,
	}

	// NamespacedNameを用いてDeploymentをcacheから取得
	if err = r.Get(ctx, deploymentNamespacedName, &deployment); err != nil {
		log.Error(err, "Unable to fetch Deployment from cache")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Nginx StatusのAvailableReplicasに関する差分比較&更新
	if nginx.Status.AvailableReplicas != deployment.Status.AvailableReplicas {
		nginx.Status.AvailableReplicas = deployment.Status.AvailableReplicas
		availableReplicasFlag = true
	}

	// Nginx StatusのDeploymentNameに関する差分比較&更新
	if nginx.Status.DeploymentName != deployment.Name {
		nginx.Status.DeploymentName = deployment.Name
		deploymentNameFlag = true
	}

	// Nginx Objectの更新(差分ありの場合)
	if availableReplicasFlag || deploymentNameFlag {
		// log.Info("Update Nginx Status.(nginx.Status.DeploymentName: " + nginx.Status.DeploymentName + " nginx.Status.AvailableReplicas: " + strconv.Itoa(int(*(nginx.Status.AvailableReplicas))))
		log.Info("Update Nginx Status.(nginx.Status.DeploymentName: " + nginx.Status.DeploymentName + ", nginx.Status.AvailableReplicas: " + strconv.Itoa(int(nginx.Status.AvailableReplicas)))
		if err = r.Status().Update(ctx, &nginx); err != nil {
			log.Error(err, "Unable to update Nginx")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// OwnerReferenceの付与状況を確認し、Indexとして付与する値を決める関数
func IndexByOwner(rawObj client.Object) []string {

	/*
	  アサーションによりclient.Object型として渡されたrawObjが*appsv1.Deployment型であることを確認しつつ、
	  *appsv1.Deployment型として値を取得
	  https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/client#Object
	*/
	deployment := rawObj.(*appsv1.Deployment)

	// OwnerReferenceへのポインタを取得
	// https://pkg.go.dev/k8s.io/apimachinery@v0.25.0/pkg/apis/meta/v1#OwnerReference
	owner := metav1.GetControllerOf(deployment)

	// OwnerReferenceが設定されていない場合
	if owner == nil {
		return nil
	}

	if owner.APIVersion != apiGVStr || owner.Kind != "Nginx" {
		return nil
	}

	return []string{owner.Name} // .metadata.controller: owner.NameというIndexを追加
}

// コントローラー起動時に実行
// SetupWithManager sets up the controller with the Manager.
func (r *NginxReconciler) SetupWithManager(mgr ctrl.Manager) error {

	/*
		Controller起動時にcache上のDeploymentにindexを付与する処理
		https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/client#FieldIndexer.IndexField
		https://book.kubebuilder.io/cronjob-tutorial/controller-implementation.html#setup
		https://zoetrope.github.io/kubebuilder-training/controller-runtime/manager.html

		IndexFieldはクラスタ上に存在する全てのDeployment(第2引数で指定したリソース)に対してindex付与の処理を行う
		  第2引数: indexの付与対象リソース(appsv1.Deployment)
		  		  第2引数はIndexFieldの仕様上、client.Object (interface)をとる
				  ここに&appsv1.Deployment{} (struct)を指定することで
				    interface = &struct
				  という形になり、実装を行っているようなイメージ
		  第3引数: indexのKey(deploymentOwnerKey=.metadata.controller)を指定(任意の文字列)
		  第4引数: indexのKeyに対するValueを決める関数(https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/client#IndexerFunc)
		          funcでの戻り値が弟3引数のdeploymentOwnerKeyというindex Keyのvalueとしてセットされる
				  クラスタ上に存在する&appsv1.Deploymentに対してclient.Objectを実装したもの(第2引数)を順次func(rawObj client.Object)に入れて処理していく感じ
	*/
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.Deployment{}, deploymentOwnerKey, IndexByOwner); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nginxv1.Nginx{}).
		Owns(&appsv1.Deployment{}). // Controllerに作成されるリソースを指定
		Complete(r)
}
