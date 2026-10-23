from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Response
from prometheus_fastapi_instrumentator import Instrumentator
from pydantic import BaseModel, Field

from controller import NavigationController

controller = NavigationController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Navigation Control API", lifespan=lifespan)
Instrumentator().instrument(app).expose(app)


class SpeedRequest(BaseModel):
    speed_knots: float = Field(..., ge=0.0, le=30.0, description="Target speed over ground, knots")


class WaypointModel(BaseModel):
    lat: float = Field(..., ge=-90.0, le=90.0)
    lon: float = Field(..., ge=-180.0, le=180.0)


class RouteRequest(BaseModel):
    waypoints: list[WaypointModel] = Field(..., min_length=2, description="Ordered route waypoints")


class AnomalyRequest(BaseModel):
    enabled: bool = Field(..., description="Whether GNSS anomaly mode is active")


@app.get("/health")
def health(response: Response) -> dict:
    health_data = controller.get_health()
    if health_data["status"] != "ok":
        response.status_code = 503
    return health_data


@app.get("/status")
def status() -> dict:
    return controller.get_status()


@app.get("/speed")
def get_speed() -> dict:
    truth = controller.get_status()["truth"]
    return {"speed_target_knots": truth["speed_target_knots"], "sog_knots": truth["sog_knots"]}


@app.post("/speed")
def set_speed(request: SpeedRequest) -> dict:
    try:
        controller.set_speed_knots(request.speed_knots)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"speed_target_knots": request.speed_knots}


@app.get("/route")
def get_route() -> dict:
    truth = controller.get_status()["truth"]
    return {"waypoints": truth["route"], "active_waypoint": truth["waypoint_index"]}


@app.post("/route")
def set_route(request: RouteRequest) -> dict:
    try:
        controller.set_route([wp.model_dump() for wp in request.waypoints])
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"waypoints": [wp.model_dump() for wp in request.waypoints]}


@app.get("/anomaly")
def get_anomaly() -> dict:
    return {"anomaly_enabled": controller.get_status()["anomaly_enabled"]}


@app.post("/anomaly")
def set_anomaly(request: AnomalyRequest) -> dict:
    controller.set_anomaly_enabled(request.enabled)
    return {"anomaly_enabled": request.enabled}


@app.get("/nmea2000/latest")
def nmea2000_latest() -> dict:
    """Most recent Actisense-ASCII frame per PGN, for quick bus inspection
    without opening a TCP connection to the gateway."""
    return {"frames": controller.get_latest_frames()}
