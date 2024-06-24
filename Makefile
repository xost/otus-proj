install:
	kubectl apply -f jaeger/jaeger-all-in-one.yaml
	kubectl replace -f jaeger/configmap.yaml
	cd auth && skaffold run
	cd account && skaffold run
	cd events &&skaffold run
	cd notif && skaffold run
	cd orders && skaffold run
	kubectl apply -f auth-ingress.yaml
	kubectl apply -f account-ingress.yaml
	kubectl apply -f events-ingress.yaml
	kubectl apply -f notif-ingress.yaml
	kubectl apply -f orders-ingress.yaml
