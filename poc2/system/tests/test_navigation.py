from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def nav_modules():
    return load_component_modules("navigation", ["vessel", "nmea2000", "sensors", "controller", "main"])


def test_vessel_math_and_model(nav_modules):
    vessel_mod = nav_modules["vessel"]

    # Wrap deg & turn math
    assert vessel_mod._wrap_deg(370.0) == 10.0
    assert vessel_mod._wrap_deg(-10.0) == 350.0
    assert vessel_mod._shortest_turn_deg(350.0, 10.0) == 20.0
    assert vessel_mod._shortest_turn_deg(10.0, 350.0) == -20.0

    # VesselModel init & step
    vessel = vessel_mod.VesselModel(speed_knots=10.0, time_scale=1.0)
    state_0 = vessel.snapshot()
    assert state_0["sog_knots"] == 10.0
    assert "latitude_deg" in state_0
    assert "longitude_deg" in state_0

    # Step simulation
    vessel.step(dt_wall_s=10.0)
    state_1 = vessel.snapshot()
    assert state_1["total_log_m"] > state_0["total_log_m"]

    # Invalid speed
    with pytest.raises(ValueError):
        vessel.set_speed_knots(-1.0)
    with pytest.raises(ValueError):
        vessel.set_speed_knots(35.0)


def test_nmea2000_encoders(nav_modules):
    n2k = nav_modules["nmea2000"]

    pos_bytes = n2k.encode_position_rapid(1.2644, 103.8200)
    assert len(pos_bytes) == 8

    cog_sog_bytes = n2k.encode_cog_sog_rapid(1, 90.0, 5.14)
    assert len(cog_sog_bytes) == 8

    heading_bytes = n2k.encode_heading(1, 180.0)
    assert len(heading_bytes) == 8

    rot_bytes = n2k.encode_rate_of_turn(1, 10.0)
    assert len(rot_bytes) == 8

    attitude_bytes = n2k.encode_attitude(1, 0.0, 1.0, 2.0)
    assert len(attitude_bytes) == 8

    wind_bytes = n2k.encode_wind(1, 10.0, 45.0)
    assert len(wind_bytes) == 7

    depth_bytes = n2k.encode_water_depth(1, 25.0)
    assert len(depth_bytes) == 8

    time_bytes = n2k.encode_time_date(19500, 3600.0)
    assert len(time_bytes) == 6

    dist_frames = n2k.encode_distance_log(19500, 3600.0, 10000.0, 500.0)
    assert len(dist_frames) >= 2


def test_navigation_controller(nav_modules):
    Controller = nav_modules["controller"].NavigationController
    controller = Controller()

    controller.set_speed_knots(12.0)
    status = controller.get_status()
    assert status["truth"]["speed_target_knots"] == 12.0

    controller.set_anomaly_enabled(True)
    assert controller.get_status()["anomaly_enabled"] is True


def test_navigation_api_endpoints(nav_modules):
    app = nav_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "truth" in res.json()

    res = client.get("/speed")
    assert res.status_code == 200

    res = client.post("/speed", json={"speed_knots": 14.5})
    assert res.status_code == 200
    assert res.json()["speed_target_knots"] == 14.5

    res = client.get("/route")
    assert res.status_code == 200
    assert "waypoints" in res.json()

    res = client.get("/anomaly")
    assert res.status_code == 200
    res = client.post("/anomaly", json={"enabled": True})
    assert res.status_code == 200
    assert res.json()["anomaly_enabled"] is True

    res = client.get("/nmea2000/latest")
    assert res.status_code == 200
    assert "frames" in res.json()
