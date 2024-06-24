# otus-task-9


операции внесения средств на баланс сервисе идемподентны

приложения деплоятся в пространство saga

```
git clone https://github.com/xost/otus-ask-9.git

cd otus-task-9

cd auth
skaffold run
cd ../account
skaffold run

```
Авторизуемся:
```
curl -v -X POST http://arch.homework/login -d '{"login":"admin","password":"password"}'
```
cookie:
```
session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d
```
Пополним баланс:
```
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X PUT http://arch.homework/account/deposit -d '{"delta":100}'
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X GET http://arch.homework/account/get
```
баланс не изменился:
```
{"balance":0}
```
пополним баланс с предварительным запросом на операцию:
запрос:
```
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X GET http://arch.homework/account/genreq
```
X-Request-Id
```
X-Request-Id: e13a61186ebc8d9ee8f92422883fc22d
```
пополним баланс с полученным request-id:
```
curl -v --header "X-Request-Id: e13a61186ebc8d9ee8f92422883fc22d" --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X POST http://arch.homework/account/deposit -d '{"delta":50}'
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X GET http://arch.homework/account/get
```
баланс:
```
{"balance":50}
```
повторим операцию пополнения с тем же request-id
```
curl -v --header "X-Request-Id: e13a61186ebc8d9ee8f92422883fc22d" --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X POST http://arch.homework/account/deposit -d '{"delta":50}'
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X GET http://arch.homework/account/get
```
баланс не изменился:
```
{"balance":50}
```
повторим операцию пополнения с произвольным request-id
```
curl -v --header "X-Request-Id: 000000" --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X POST http://arch.homework/account/deposit -d '{"delta":50}'
curl -v --cookie session_id=d6b74026-7bdf-4e31-8955-75fe1e0d075d -X GET http://arch.homework/account/get
```
баланс не изменился:
```
{"balance":50}
```

Вывод: операция внесения средств идемподентна.


