from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Response
from pydantic import BaseModel, Field

from controller import BatteryController

controller = BatteryController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Battery Control API", lifespan=lifespan)


class LoadRequest(BaseModel):
    load_ratio: float = Field(..., ge=0.0, le=1.0, description="Target discharge ratio, 0.0-1.0")


class ChargeRequest(BaseModel):
    power_kw: float = Field(
        ..., ge=0.0, description="Target charge power in kW, clamped to the battery's max charging power"
    )


@app.get("/health")
def health(response: Response) -> dict:
    health_data = controller.get_health()
    if health_data["status"] != "ok":
        response.status_code = 503
    return health_data


@app.get("/status")
def status() -> dict:
    return controller.get_status()


@app.get("/load")
def get_load() -> dict:
    status_data = controller.get_status()
    return {
        "target_load_ratio": status_data["target_load_ratio"],
        "current_load_ratio": status_data["current_load_ratio"],
        "soc": status_data["soc"],
    }


@app.post("/load")
def set_load(request: LoadRequest) -> dict:
    try:
        controller.set_target_load_ratio(request.load_ratio)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"target_load_ratio": request.load_ratio}


@app.get("/charge")
def get_charge() -> dict:
    status_data = controller.get_status()
    return {
        "target_charge_power_kw": status_data["target_charge_power_kw"],
        "current_charge_power_kw": status_data["current_charge_power_kw"],
        "max_charging_power_kw": status_data["max_charging_power_kw"],
        "soc": status_data["soc"],
    }


@app.post("/charge")
def set_charge(request: ChargeRequest) -> dict:
    try:
        controller.set_target_charge_power_kw(request.power_kw)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"target_charge_power_kw": request.power_kw}


@app.get("/prediction")
def get_prediction() -> dict:
    status_data = controller.get_status()
    return {
        "soc": status_data["soc"],
        "soc_rate_per_hour": status_data["soc_rate_per_hour"],
        "time_to_empty_hr": status_data.get("time_to_empty_hr"),
        "time_to_full_hr": status_data.get("time_to_full_hr"),
    }
