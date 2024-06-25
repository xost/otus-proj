install:
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

delete:
	cd auth && skaffold delete
	cd account && skaffold delete
	cd events &&skaffold delete
	cd notif && skaffold delete
	cd orders && skaffold delete
	kubectl delete -f auth-ingress.yaml
	kubectl delete -f account-ingress.yaml
	kubectl delete -f events-ingress.yaml
	kubectl delete -f notif-ingress.yaml
	kubectl delete -f orders-ingress.yaml


