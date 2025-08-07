#!/bin/bash
curl -X GET \
  -H "Authorization: Bearer 4343afe34e324093253465464523342343242132112" \
  "http://localhost:9201/api/v1/blueprints/$1/raw"
